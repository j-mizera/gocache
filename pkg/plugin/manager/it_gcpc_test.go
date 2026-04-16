package manager

import (
	"context"
	"net"
	"testing"
	"time"

	opctx "gocache/api/context"
	apiEvents "gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	ops "gocache/api/operations"
	"gocache/api/transport"
	serverEvents "gocache/pkg/events"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/plugin"
)

// testPlugin connects to the manager via GCPC and exercises all message types.
type testPlugin struct {
	t    *testing.T
	conn *transport.Conn
}

func newTestPlugin(t *testing.T, sockPath string) *testPlugin {
	t.Helper()
	raw, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial plugin socket: %v", err)
	}
	tc := transport.NewConn(raw)
	t.Cleanup(func() { tc.Close() })
	return &testPlugin{t: t, conn: tc}
}

func (p *testPlugin) register(name string, scopes []string, hooks []*gcpc.HookDeclV1, opHooks []*gcpc.OperationHookDeclV1) *gcpc.RegisterAckV1 {
	p.t.Helper()
	reg := &gcpc.RegisterV1{
		Name:            name,
		Version:         "0.1.0-test",
		Critical:        false,
		RequestedScopes: scopes,
		Hooks:           hooks,
		OperationHooks:  opHooks,
	}
	env := &gcpc.EnvelopeV1{
		Version: gcpc.ProtocolVersion,
		Payload: &gcpc.EnvelopeV1_Register{Register: reg},
	}
	if err := p.conn.Send(env); err != nil {
		p.t.Fatalf("send register: %v", err)
	}
	ackEnv, err := p.conn.Recv()
	if err != nil {
		p.t.Fatalf("recv ack: %v", err)
	}
	ack := ackEnv.GetRegisterAck()
	if ack == nil {
		p.t.Fatal("expected RegisterAck")
	}
	return ack
}

func (p *testPlugin) recv() *gcpc.EnvelopeV1 {
	p.t.Helper()
	env, err := p.conn.Recv()
	if err != nil {
		p.t.Fatalf("recv: %v", err)
	}
	return env
}

func (p *testPlugin) send(env *gcpc.EnvelopeV1) {
	p.t.Helper()
	if err := p.conn.Send(env); err != nil {
		p.t.Fatalf("send: %v", err)
	}
}

// waitForSubscription polls the event bus until the named subscriber appears
// or the timeout expires. Avoids flaky time.Sleep-based synchronization with
// the async subscription registration in the manager's readLoop.
func waitForSubscription(t *testing.T, bus *serverEvents.Bus, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bus.HasSubscriber(name) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("subscription %q not registered within timeout", name)
}

// setupManager creates a manager with a temp socket and returns it ready for connections.
func setupManager(t *testing.T) (*Manager, *serverEvents.Bus, *serverOps.Tracker, string) {
	t.Helper()

	sockPath := t.TempDir() + "/test.sock"
	cfg := plugin.PluginsConfig{
		Enabled:         true,
		SocketPath:      sockPath,
		HealthInterval:  60 * time.Second, // long so it doesn't interfere with tests
		ShutdownTimeout: 5 * time.Second,
		MaxRestarts:     0,
		ConnectTimeout:  5 * time.Second,
	}

	tracker := serverOps.NewTracker()
	eventBus := serverEvents.NewBus()

	mgr := NewManager(cfg, []string{"GET", "SET", "PING"}, &mockState{})
	mgr.SetEventBus(eventBus)

	// Set the context (normally done by Start()).
	ctx, cancel := context.WithCancel(context.Background())
	mgr.ctx = ctx
	mgr.cancel = cancel
	t.Cleanup(cancel)

	// Pre-register a plugin entry so the manager accepts the connection.
	mgr.registry.Add(&PluginInstance{
		Name:        "test-plugin",
		MaxRestarts: 0,
	})

	// Start the IPC listener.
	listener, err := transport.NewListener(sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	// Accept connections in background.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go mgr.handleConnection(conn)
		}
	}()

	return mgr, eventBus, tracker, sockPath
}

type mockState struct{}

func (m *mockState) IsShuttingDown() bool   { return false }
func (m *mockState) StartTime() time.Time   { return time.Now() }
func (m *mockState) ActiveConnections() int { return 0 }
func (m *mockState) CacheKeys() int         { return 0 }
func (m *mockState) CacheUsedBytes() int64  { return 0 }
func (m *mockState) CacheMaxBytes() int64   { return 0 }

// --- Integration Tests ---

func TestGCPC_Registration_Accepted(t *testing.T) {
	_, _, _, sockPath := setupManager(t)
	p := newTestPlugin(t, sockPath)

	ack := p.register("test-plugin", []string{"read"}, nil, nil)

	if !ack.Accepted {
		t.Fatalf("expected accepted, got rejected: %s", ack.Reason)
	}
	if len(ack.GrantedScopes) == 0 {
		t.Error("expected granted scopes")
	}
}

func TestGCPC_Registration_UnknownPlugin(t *testing.T) {
	_, _, _, sockPath := setupManager(t)
	p := newTestPlugin(t, sockPath)

	ack := p.register("nonexistent", []string{"read"}, nil, nil)

	if ack.Accepted {
		t.Error("expected rejection for unknown plugin")
	}
}

func TestGCPC_Registration_PartialScopeGrant(t *testing.T) {
	_, _, _, sockPath := setupManager(t)
	p := newTestPlugin(t, sockPath)

	// Request scopes beyond what's allowed (default allows "read" only).
	ack := p.register("test-plugin", []string{"read", "write", "admin", "operation:hook"}, nil, nil)

	if !ack.Accepted {
		t.Fatalf("expected accepted with partial grant, got rejected: %s", ack.Reason)
	}
	// Only "read" should be granted (default allowlist).
	granted := make(map[string]bool)
	for _, s := range ack.GrantedScopes {
		granted[s] = true
	}
	if !granted["read"] {
		t.Error("expected read scope granted")
	}
}

func TestGCPC_OperationHook_Registration(t *testing.T) {
	mgr, _, _, sockPath := setupManager(t)
	mgr.cfg.Overrides = map[string]plugin.PluginOverride{
		"test-plugin": {Scopes: []string{"read", "operation:hook"}},
	}
	p := newTestPlugin(t, sockPath)

	ack := p.register("test-plugin", []string{"read", "operation:hook"}, nil,
		[]*gcpc.OperationHookDeclV1{{Type: "*", Priority: 10}})

	if !ack.Accepted {
		t.Fatalf("registration rejected: %s", ack.Reason)
	}

	// Verify operation hooks were registered.
	if !mgr.opHookRegistry.HasAny() {
		t.Fatal("expected operation hooks registered")
	}

	// Verify wildcard match works.
	matches := mgr.opHookRegistry.Match(ops.TypeCommand)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].PluginName != "test-plugin" {
		t.Errorf("expected test-plugin, got %s", matches[0].PluginName)
	}
}

func TestGCPC_OperationHook_DeniedWithoutScope(t *testing.T) {
	mgr, _, _, sockPath := setupManager(t)
	// Default scopes — no operation:hook.
	p := newTestPlugin(t, sockPath)

	p.register("test-plugin", []string{"read"}, nil,
		[]*gcpc.OperationHookDeclV1{{Type: "*", Priority: 10}})

	// Hooks should NOT be registered.
	if mgr.opHookRegistry.HasAny() {
		t.Error("operation hooks should not be registered without scope")
	}
}

func TestGCPC_EventSubscription(t *testing.T) {
	mgr, eventBus, _, sockPath := setupManager(t)
	mgr.cfg.Overrides = map[string]plugin.PluginOverride{
		"test-plugin": {Scopes: []string{"read", "events"}},
	}
	p := newTestPlugin(t, sockPath)

	ack := p.register("test-plugin", []string{"read", "events"}, nil, nil)
	if !ack.Accepted {
		t.Fatalf("rejected: %s", ack.Reason)
	}

	// Subscribe to command.post events.
	p.send(gcpc.NewEventSubscribe([]string{"command.post"}))
	waitForSubscription(t, eventBus, "plugin:test-plugin")

	// Emit a command.post event.
	eventBus.Emit(apiEvents.NewCommandPost("SET", []string{"key", "val"}, 1000, "OK", "", nil).WithOperationID("cmd_1"))

	// Plugin should receive it.
	env := p.recv()
	evt := env.GetEvent()
	if evt == nil {
		t.Fatal("expected EventV1")
	}
	if evt.Type != "command.post" {
		t.Errorf("expected command.post, got %q", evt.Type)
	}
	if evt.OperationId != "cmd_1" {
		t.Errorf("expected operation_id cmd_1, got %q", evt.OperationId)
	}

	cmdPost := evt.GetCommandPost()
	if cmdPost == nil {
		t.Fatal("expected CommandPostEventV1")
	}
	if cmdPost.Command != "SET" {
		t.Errorf("expected SET, got %q", cmdPost.Command)
	}
	if cmdPost.ElapsedNs != 1000 {
		t.Errorf("expected elapsed 1000, got %d", cmdPost.ElapsedNs)
	}
}

func TestGCPC_EventSubscription_ContextFiltered(t *testing.T) {
	mgr, eventBus, _, sockPath := setupManager(t)
	mgr.cfg.Overrides = map[string]plugin.PluginOverride{
		"test-plugin": {Scopes: []string{"read", "events"}},
	}
	p := newTestPlugin(t, sockPath)

	p.register("test-plugin", []string{"read", "events"}, nil, nil)
	p.send(gcpc.NewEventSubscribe([]string{"operation.complete"}))
	waitForSubscription(t, eventBus, "plugin:test-plugin")

	// Emit operation.complete with context containing private keys.
	ctx := map[string]string{
		"_start_ns":            "12345",
		"shared.traceparent":   "00-abc-def-01",
		"other-plugin.private": "should-be-hidden",
		"test-plugin.own":      "should-be-visible",
	}
	eventBus.Emit(apiEvents.NewOperationComplete("cmd_1", "command", 5000, "completed", "", ctx))

	env := p.recv()
	evt := env.GetEvent()
	opComplete := evt.GetOperationComplete()
	if opComplete == nil {
		t.Fatal("expected OperationCompleteEventV1")
	}

	// Context should be filtered for test-plugin.
	if opComplete.Context["_start_ns"] != "12345" {
		t.Error("expected _start_ns (server key)")
	}
	if opComplete.Context["shared.traceparent"] != "00-abc-def-01" {
		t.Error("expected shared.traceparent")
	}
	if opComplete.Context["test-plugin.own"] != "should-be-visible" {
		t.Error("expected own key visible")
	}
	if _, ok := opComplete.Context["other-plugin.private"]; ok {
		t.Error("should NOT see other-plugin.private")
	}
}

func TestGCPC_EventSubscription_DeniedWithoutScope(t *testing.T) {
	_, eventBus, _, sockPath := setupManager(t)
	p := newTestPlugin(t, sockPath)

	// Register without events scope.
	ack := p.register("test-plugin", []string{"read"}, nil, nil)
	if !ack.Accepted {
		t.Fatalf("rejected: %s", ack.Reason)
	}

	// Try to subscribe — should be silently denied.
	p.send(gcpc.NewEventSubscribe([]string{"command.post"}))

	// Poll briefly to let the manager process the subscribe message, then
	// assert the subscription was NOT registered on the bus.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if eventBus.HasSubscriber("plugin:test-plugin") {
			t.Fatal("subscription should have been denied but was registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGCPC_ServerQuery(t *testing.T) {
	mgr, _, _, sockPath := setupManager(t)
	mgr.cfg.Overrides = map[string]plugin.PluginOverride{
		"test-plugin": {Scopes: []string{"read", "server:query"}},
	}
	p := newTestPlugin(t, sockPath)

	ack := p.register("test-plugin", []string{"read", "server:query"}, nil, nil)
	if !ack.Accepted {
		t.Fatalf("rejected: %s", ack.Reason)
	}

	// Query health topic.
	p.send(gcpc.NewServerQuery("q-1", "health"))

	env := p.recv()
	queryResp := env.GetServerQueryResponse()
	if queryResp == nil {
		t.Fatal("expected ServerQueryResponseV1")
	}
	if queryResp.RequestId != "q-1" {
		t.Errorf("expected request_id q-1, got %q", queryResp.RequestId)
	}
	if queryResp.Error != "" {
		t.Errorf("unexpected error: %s", queryResp.Error)
	}
	if queryResp.Data["status"] != "ok" {
		t.Errorf("expected status ok, got %q", queryResp.Data["status"])
	}
}

func TestGCPC_ServerQuery_DeniedScope(t *testing.T) {
	_, _, _, sockPath := setupManager(t)
	p := newTestPlugin(t, sockPath)

	// Register without server:query scope.
	ack := p.register("test-plugin", []string{"read"}, nil, nil)
	if !ack.Accepted {
		t.Fatalf("rejected: %s", ack.Reason)
	}

	// Query should be denied.
	p.send(gcpc.NewServerQuery("q-2", "health"))

	env := p.recv()
	queryResp := env.GetServerQueryResponse()
	if queryResp == nil {
		t.Fatal("expected ServerQueryResponseV1")
	}
	if queryResp.Error == "" {
		t.Error("expected permission denied error")
	}
}

func TestGCPC_ServerQuery_Plugins(t *testing.T) {
	mgr, _, _, sockPath := setupManager(t)
	mgr.cfg.Overrides = map[string]plugin.PluginOverride{
		"test-plugin": {Scopes: []string{"read", "server:query"}},
	}
	p := newTestPlugin(t, sockPath)

	p.register("test-plugin", []string{"read", "server:query"}, nil, nil)

	// Query plugins topic.
	p.send(gcpc.NewServerQuery("q-3", "plugins"))

	env := p.recv()
	queryResp := env.GetServerQueryResponse()
	if queryResp.Error != "" {
		t.Fatalf("unexpected error: %s", queryResp.Error)
	}
	// Should show test-plugin as running.
	if queryResp.Data["test-plugin.state"] != "running" {
		t.Errorf("expected test-plugin running, got %q", queryResp.Data["test-plugin.state"])
	}
}

func TestGCPC_SecretRedaction_InContextSnapshot(t *testing.T) {
	// Verify that RedactSecrets works across all namespaces.
	ctx := map[string]string{
		"_start_ns":           "123",
		"_secret.session":     "tok",
		"shared.username":     "john",
		"shared.secret.jwt":   "eyJ...",
		"auth.cache_hit":      "true",
		"auth.secret.api_key": "key123",
	}

	redacted := opctx.RedactSecrets(ctx)

	if _, ok := redacted["_secret.session"]; ok {
		t.Error("_secret.session should be redacted")
	}
	if _, ok := redacted["shared.secret.jwt"]; ok {
		t.Error("shared.secret.jwt should be redacted")
	}
	if _, ok := redacted["auth.secret.api_key"]; ok {
		t.Error("auth.secret.api_key should be redacted")
	}
	if redacted["_start_ns"] != "123" {
		t.Error("non-secret should survive")
	}
	if redacted["shared.username"] != "john" {
		t.Error("non-secret should survive")
	}
}
