package hooks

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"gocache/pkg/plugin/protocol"
	"gocache/pkg/plugin/router"
	"gocache/pkg/plugin/transport"
	gcpc "gocache/proto/gcpc/v1"
)

func testPipe() (*transport.Conn, *transport.Conn) {
	server, client := net.Pipe()
	return transport.NewConn(server), transport.NewConn(client)
}

func TestRegistryBasic(t *testing.T) {
	r := NewRegistry()

	if r.HasAny() {
		t.Error("expected HasAny=false on empty registry")
	}

	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	pc := router.NewPluginConn("auth", serverConn)
	defer pc.Close()

	decls := []*gcpc.HookDeclV1{
		{Pattern: "SET", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
		{Pattern: "*", Phase: gcpc.HookPhaseV1_HOOK_PHASE_POST},
	}
	r.Register("auth", 1, true, pc, decls)

	if !r.HasAny() {
		t.Error("expected HasAny=true after registration")
	}

	// Pre-hooks: only "SET" pattern matches SET.
	pre := r.MatchPre("SET")
	if len(pre) != 1 {
		t.Fatalf("expected 1 pre-hook for SET, got %d", len(pre))
	}
	if pre[0].PluginName != "auth" {
		t.Errorf("expected plugin 'auth', got %s", pre[0].PluginName)
	}

	// No pre-hook for GET.
	pre = r.MatchPre("GET")
	if len(pre) != 0 {
		t.Errorf("expected 0 pre-hooks for GET, got %d", len(pre))
	}

	// Post-hooks: wildcard matches everything.
	post := r.MatchPost("SET")
	if len(post) != 1 {
		t.Fatalf("expected 1 post-hook for SET, got %d", len(post))
	}
	post = r.MatchPost("GET")
	if len(post) != 1 {
		t.Fatalf("expected 1 post-hook for GET, got %d", len(post))
	}
}

func TestRegistryCaseInsensitive(t *testing.T) {
	r := NewRegistry()
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	pc := router.NewPluginConn("test", serverConn)
	defer pc.Close()

	r.Register("test", 1, true, pc, []*gcpc.HookDeclV1{
		{Pattern: "set", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})

	if len(r.MatchPre("SET")) != 1 {
		t.Error("expected case-insensitive pattern match")
	}
	if len(r.MatchPre("set")) != 1 {
		t.Error("expected case-insensitive command match")
	}
}

func TestRegistryPriorityOrder(t *testing.T) {
	r := NewRegistry()

	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()
	s2, c2 := testPipe()
	defer c2.Close()
	defer s2.Close()
	s3, c3 := testPipe()
	defer c3.Close()
	defer s3.Close()

	pc1 := router.NewPluginConn("low", s1)
	defer pc1.Close()
	pc2 := router.NewPluginConn("high", s2)
	defer pc2.Close()
	pc3 := router.NewPluginConn("mid", s3)
	defer pc3.Close()

	r.Register("low", 10, false, pc1, []*gcpc.HookDeclV1{
		{Pattern: "*", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})
	r.Register("high", 1, true, pc2, []*gcpc.HookDeclV1{
		{Pattern: "*", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})
	r.Register("mid", 5, false, pc3, []*gcpc.HookDeclV1{
		{Pattern: "*", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})

	pre := r.MatchPre("SET")
	if len(pre) != 3 {
		t.Fatalf("expected 3 hooks, got %d", len(pre))
	}
	if pre[0].PluginName != "high" || pre[1].PluginName != "mid" || pre[2].PluginName != "low" {
		t.Errorf("expected priority order high,mid,low — got %s,%s,%s", pre[0].PluginName, pre[1].PluginName, pre[2].PluginName)
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := NewRegistry()
	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()

	pc := router.NewPluginConn("audit", s1)
	defer pc.Close()

	r.Register("audit", 5, false, pc, []*gcpc.HookDeclV1{
		{Pattern: "*", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
		{Pattern: "*", Phase: gcpc.HookPhaseV1_HOOK_PHASE_POST},
	})

	if !r.HasAny() {
		t.Fatal("expected hooks registered")
	}

	r.Unregister("audit")

	if r.HasAny() {
		t.Error("expected no hooks after unregister")
	}
	if len(r.MatchPre("SET")) != 0 {
		t.Error("expected no pre-hooks after unregister")
	}
	if len(r.MatchPost("SET")) != 0 {
		t.Error("expected no post-hooks after unregister")
	}
}

func TestExecutorPreHookDeny(t *testing.T) {
	reg := NewRegistry()
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	pc := router.NewPluginConn("acl", serverConn)
	defer pc.Close()

	reg.Register("acl", 1, true, pc, []*gcpc.HookDeclV1{
		{Pattern: "SET", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})

	exec := NewExecutor(reg, 5*time.Second)

	// Plugin side: read hook request, send deny response.
	go func() {
		env, err := clientConn.Recv()
		if err != nil {
			t.Errorf("plugin recv: %v", err)
			return
		}
		req := env.GetHookRequest()
		if req == nil {
			t.Error("expected HookRequest")
			return
		}
		if req.Command != "SET" {
			t.Errorf("expected SET, got %s", req.Command)
		}
		resp := protocol.NewHookResponse(req.RequestId, true, "permission denied", nil)
		if err := clientConn.Send(resp); err != nil {
			t.Errorf("plugin send: %v", err)
		}
	}()

	ctx := context.Background()
	result := exec.RunPreHooks(ctx, "SET", []string{"key", "val"}, nil)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Denied {
		t.Error("expected command to be denied")
	}
	if result.DenyReason != "permission denied" {
		t.Errorf("expected 'permission denied', got %q", result.DenyReason)
	}

	clientConn.Close()
}

func TestExecutorPreHookAllow(t *testing.T) {
	reg := NewRegistry()
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	pc := router.NewPluginConn("acl", serverConn)
	defer pc.Close()

	reg.Register("acl", 1, true, pc, []*gcpc.HookDeclV1{
		{Pattern: "SET", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})

	exec := NewExecutor(reg, 5*time.Second)

	// Plugin allows the command.
	go func() {
		env, _ := clientConn.Recv()
		req := env.GetHookRequest()
		resp := protocol.NewHookResponse(req.RequestId, false, "", nil)
		_ = clientConn.Send(resp)
	}()

	result := exec.RunPreHooks(context.Background(), "SET", []string{"key", "val"}, nil)
	if result == nil || result.Denied {
		t.Error("expected command to be allowed")
	}

	clientConn.Close()
}

func TestExecutorNonCriticalFireAndForget(t *testing.T) {
	reg := NewRegistry()
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	pc := router.NewPluginConn("audit", serverConn)
	defer pc.Close()

	// Non-critical hook (critical=false).
	reg.Register("audit", 10, false, pc, []*gcpc.HookDeclV1{
		{Pattern: "*", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})

	exec := NewExecutor(reg, 5*time.Second)

	// Track if the plugin received the hook.
	var received atomic.Bool
	go func() {
		env, err := clientConn.Recv()
		if err != nil {
			return
		}
		if env.GetHookRequest() != nil {
			received.Store(true)
		}
	}()

	// Non-critical pre-hooks should not block — returns immediately.
	result := exec.RunPreHooks(context.Background(), "SET", []string{"key", "val"}, nil)
	if result == nil || result.Denied {
		t.Error("non-critical hook should not deny")
	}

	// Give the async goroutine time to deliver.
	time.Sleep(100 * time.Millisecond)
	if !received.Load() {
		t.Error("expected non-critical hook to be received by plugin")
	}

	clientConn.Close()
}

func TestExecutorNoHooksZeroCost(t *testing.T) {
	reg := NewRegistry()
	exec := NewExecutor(reg, time.Second)

	if exec.HasAny() {
		t.Error("expected HasAny=false")
	}

	// Should return nil immediately — no allocations, no goroutines.
	result := exec.RunPreHooks(context.Background(), "SET", []string{"key", "val"}, nil)
	if result != nil {
		t.Error("expected nil result when no hooks")
	}
}

func TestExecutorPostHook(t *testing.T) {
	reg := NewRegistry()
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	pc := router.NewPluginConn("aof", serverConn)
	defer pc.Close()

	// Critical post-hook.
	reg.Register("aof", 5, true, pc, []*gcpc.HookDeclV1{
		{Pattern: "SET", Phase: gcpc.HookPhaseV1_HOOK_PHASE_POST},
	})

	exec := NewExecutor(reg, 5*time.Second)

	// Plugin side: read hook, verify it has result context, send ack.
	go func() {
		env, _ := clientConn.Recv()
		req := env.GetHookRequest()
		if req.Phase != gcpc.HookPhaseV1_HOOK_PHASE_POST {
			t.Errorf("expected POST phase, got %v", req.Phase)
		}
		if req.ResultValue != "OK" {
			t.Errorf("expected result 'OK', got %q", req.ResultValue)
		}
		resp := protocol.NewHookResponse(req.RequestId, false, "", nil)
		_ = clientConn.Send(resp)
	}()

	exec.RunPostHooks(context.Background(), "SET", []string{"key", "val"}, "OK", "", nil)

	clientConn.Close()
}

func TestExecutorPreHookTimeout(t *testing.T) {
	reg := NewRegistry()
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	pc := router.NewPluginConn("slow", serverConn)
	defer pc.Close()

	reg.Register("slow", 1, true, pc, []*gcpc.HookDeclV1{
		{Pattern: "SET", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})

	// Short timeout — plugin never responds.
	exec := NewExecutor(reg, 50*time.Millisecond)

	// Plugin side: read but don't respond.
	go func() {
		_, _ = clientConn.Recv()
		// intentionally don't respond
	}()

	// Should fail-open (allow command) after timeout.
	result := exec.RunPreHooks(context.Background(), "SET", []string{"key", "val"}, nil)
	if result == nil || result.Denied {
		t.Error("expected fail-open on timeout (command allowed)")
	}
}

func TestExecutorNoMatchReturnsNil(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("auth", s)
	defer pc.Close()

	// Only hooks SET.
	reg.Register("auth", 1, true, pc, []*gcpc.HookDeclV1{
		{Pattern: "SET", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE},
	})

	exec := NewExecutor(reg, time.Second)

	// GET should not trigger hooks.
	result := exec.RunPreHooks(context.Background(), "GET", []string{"key"}, nil)
	if result != nil {
		t.Error("expected nil for non-matching command")
	}
}
