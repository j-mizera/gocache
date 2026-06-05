package pipeline

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"

	apicommand "gocache/api/command"
	apiEvents "gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	apiobs "gocache/api/observability"
	ops "gocache/api/operations"
	commonobs "gocache/commons/observability"
	"gocache/commons/transport"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	pluginrouter "gocache/pkg/plugin/router"
	"gocache/pkg/watch"
)

// --- Test helpers ---

func newTestPipeline() (*Pipeline, *engine.Engine, *commonobs.SlotOperationTrackerManager) {
	c := cache.New()
	e := engine.New(c)
	br := blocking.NewRegistry()
	wm := watch.NewManager()
	eval := New(c, e, "", br, wm)
	tracker := newPipelineTelemetryTracker()
	eval.SetOperationTrackerManager(tracker)
	return eval, e, tracker
}

// mockHookExecutor implements command.HookExecutor for testing.
type mockHookExecutor struct {
	hasAny      bool
	preResult   *apicommand.PreHookResult
	postCalled  atomic.Int32
	lastHookCtx map[string]string
}

func (m *mockHookExecutor) HasAny() bool                           { return m.hasAny }
func (m *mockHookExecutor) HasHooksForCommand(command string) bool { return m.hasAny }
func (m *mockHookExecutor) RunPreHooks(_ context.Context, _ *gcpc.CommandInfoV1, _ *gcpc.ConnectionInfoV1, hookCtx map[string]string) *apicommand.PreHookResult {
	m.lastHookCtx = hookCtx
	return m.preResult
}
func (m *mockHookExecutor) RunPostHooks(_ context.Context, _ *gcpc.CommandInfoV1, _ *gcpc.ConnectionInfoV1, _, _ string, _ map[string]string) {
	m.postCalled.Add(1)
}

// mockOpHookExecutor implements OpHookExecutor for testing.
type mockOpHookExecutor struct {
	hasAny         bool
	startCalled    atomic.Int32
	completeCalled atomic.Int32
	lastOp         atomic.Pointer[ops.Operation]
	enrichOnStart  map[string]string // context values to add during start
}

func (m *mockOpHookExecutor) HasAny() bool                          { return m.hasAny }
func (m *mockOpHookExecutor) HasOperationType(opType ops.Type) bool { return m.hasAny }
func (m *mockOpHookExecutor) RunStartHooks(_ context.Context, op *ops.Operation) {
	m.startCalled.Add(1)
	m.lastOp.Store(op)
	if m.enrichOnStart != nil {
		op.EnrichMany(m.enrichOnStart)
	}
}
func (m *mockOpHookExecutor) RunCompleteHooks(op *ops.Operation) {
	m.completeCalled.Add(1)
	m.lastOp.Store(op)
}

// mockEmitter collects emitted events. Reports as having subscribers so the
// evaluator takes the slow (instrumented) path — that is the path these
// tests exercise.
type mockEmitter struct {
	events []apiEvents.Event
}

func (m *mockEmitter) Emit(evt apiEvents.Event) {
	m.events = append(m.events, evt)
}

func (m *mockEmitter) HasSubscribers() bool                           { return true }
func (m *mockEmitter) HasSubscribersFor(types ...apiEvents.Type) bool { return true }

type commandScopeContextKey struct{}

type drainedTelemetryOperation struct {
	operation      apiobs.InternalOperationIdentity
	parent         string
	contextVersion apiobs.ConnectionContextVersion
	contextOverlay map[string]string
	status         commonobs.SlotTerminalStatus
	records        []apiobs.TelemetryRecord
}

func countEventRecords(records []apiobs.TelemetryRecord) int {
	count := 0
	for _, record := range records {
		switch record.Kind {
		case apiobs.TelemetryRecordOperationStart,
			apiobs.TelemetryRecordOperationFinish,
			apiobs.TelemetryRecordCommandStart,
			apiobs.TelemetryRecordCommandFinish,
			apiobs.TelemetryRecordEvent:
			count++
		}
	}
	return count
}

func telemetryRecordStringField(record apiobs.TelemetryRecord, wantKey string) (string, bool) {
	payload := record.PayloadBytes()
	if len(payload) == 0 {
		return "", false
	}
	pos := 1
	for count := int(payload[0]); count > 0; count-- {
		if pos >= len(payload) {
			return "", false
		}
		keyLen := int(payload[pos])
		pos++
		if pos+keyLen > len(payload) {
			return "", false
		}
		key := string(payload[pos : pos+keyLen])
		pos += keyLen
		if pos >= len(payload) {
			return "", false
		}
		valueLen := int(payload[pos])
		pos++
		if pos+valueLen > len(payload) {
			return "", false
		}
		value := string(payload[pos : pos+valueLen])
		pos += valueLen
		if key == wantKey {
			return value, true
		}
	}
	return "", false
}

func newPipelineTelemetryTracker() *commonobs.SlotOperationTrackerManager {
	return commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           64,
		RecordsPerOperation:   8,
		CompletedRingPerShard: 64,
	})
}

func drainTelemetryOperations(manager *commonobs.SlotOperationTrackerManager) []drainedTelemetryOperation {
	out := make([]drainedTelemetryOperation, 0, manager.ShardCount())
	for shard := range manager.ShardCount() {
		manager.DrainCompletedShard(shard, func(completed commonobs.CompletedOperation) {
			records := append([]apiobs.TelemetryRecord(nil), completed.Records...)
			var contextOverlay map[string]string
			if completed.ContextOverlay != nil {
				contextOverlay = make(map[string]string, len(completed.ContextOverlay))
				for key, value := range completed.ContextOverlay {
					contextOverlay[key] = value
				}
			}
			out = append(out, drainedTelemetryOperation{
				operation:      completed.Operation,
				parent:         completed.Parent.String(),
				contextVersion: completed.ContextVersion,
				contextOverlay: contextOverlay,
				status:         completed.Status,
				records:        records,
			})
		})
	}
	return out
}

// --- Tests ---

func TestEvaluate_BasicCommand(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	ctx := clientctx.New()
	result := eval.Evaluate(context.Background(), ctx, "PING", nil)
	if result.Value != "PONG" {
		t.Errorf("expected PONG, got %v", result.Value)
	}
}

func TestEvaluate_CommandScopeUsesCancellationContextAcrossPaths(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Pipeline) *mockCommandMetricsRecorder
	}{
		{
			name: "fast",
		},
		{
			name: "metrics-only",
			configure: func(eval *Pipeline) *mockCommandMetricsRecorder {
				recorder := &mockCommandMetricsRecorder{active: true}
				eval.SetCommandMetricsRecorder(recorder)
				return recorder
			},
		},
		{
			name: "full",
			configure: func(eval *Pipeline) *mockCommandMetricsRecorder {
				eval.SetEmitter(&mockEmitter{})
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval, e, _ := newTestPipeline()
			defer e.Stop()

			manager := newPipelineTelemetryTracker()
			eval.SetOperationTrackerManager(manager)
			var recorder *mockCommandMetricsRecorder
			if tt.configure != nil {
				recorder = tt.configure(eval)
			}

			marker := "marker-" + tt.name
			parentCtx := context.WithValue(context.Background(), commandScopeContextKey{}, marker)
			client := clientctx.New()
			client.OperationID = "parent-" + tt.name
			handlerCalled := false

			eval.RegisterHandler("SCOPE", func(cmdCtx *command.Context) apicommand.Result {
				handlerCalled = true
				if got := cmdCtx.CancellationContext().Value(commandScopeContextKey{}); got != marker {
					t.Errorf("cancellation context value = %v, want %q", got, marker)
				}
				if op := ops.FromContext(cmdCtx.CancellationContext()); op != nil {
					t.Errorf("cancellation context carried legacy operation %q", op.ID)
				}
				if cmdCtx.Context() != cmdCtx.CancellationContext() {
					t.Error("Context compatibility alias should return cancellation context")
				}
				if cmdCtx.Telemetry().IsZero() {
					t.Fatal("expected non-zero command telemetry scope")
				}
				record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 0)
				record.SetNameString("scope")
				if !cmdCtx.RecordTelemetry(record) {
					t.Error("expected command telemetry record to be accepted")
				}
				if !cmdCtx.Log(apiobs.TelemetryLogLevelInfo, []byte("handler log")) {
					t.Error("expected command telemetry log to be accepted")
				}
				return apicommand.Result{Value: "OK"}
			})

			result := eval.Evaluate(parentCtx, client, "SCOPE", nil)
			if result.Value != "OK" {
				t.Fatalf("result = %v, want OK", result.Value)
			}
			if !handlerCalled {
				t.Fatal("handler was not called")
			}
			if recorder != nil && len(recorder.commands) != 1 {
				t.Fatalf("metrics recorder calls = %d, want 1", len(recorder.commands))
			}

			drained := drainTelemetryOperations(manager)
			if len(drained) != 1 {
				t.Fatalf("drained operations = %d, want 1", len(drained))
			}
			completed := drained[0]
			if completed.operation.IsZero() {
				t.Fatal("completed operation identity is zero")
			}
			if completed.parent != client.OperationID {
				t.Fatalf("parent = %q, want %q", completed.parent, client.OperationID)
			}
			if completed.status != commonobs.SlotTerminalFinished {
				t.Fatalf("status = %v, want finished", completed.status)
			}
			wantRecords := 4
			if tt.name == "full" {
				wantRecords = 8
			}
			if len(completed.records) != wantRecords {
				t.Fatalf("records = %d, want %d", len(completed.records), wantRecords)
			}
			for i, record := range completed.records {
				if record.Operation != completed.operation {
					t.Fatalf("record[%d] operation = %d, want %d", i, record.Operation, completed.operation)
				}
			}
		})
	}
}

func TestEvaluate_CommandScopePinsConnectionContextVersionUntilDrain(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	manager := newPipelineTelemetryTracker()
	eval.SetOperationTrackerManager(manager)

	connection := apiobs.ConnectionIdentity(77)
	startVersion := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	client := clientctx.New()
	client.ConnectionIdentity = connection
	client.OperationID = "parent-pin"
	handlerCalled := false
	var currentVersion apiobs.ConnectionContextVersion

	eval.RegisterHandler("PINCTX", func(cmdCtx *command.Context) apicommand.Result {
		handlerCalled = true
		currentVersion = manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("globex"))
		if currentVersion == startVersion {
			t.Fatal("context update should create a new current version")
		}
		gotStart := map[string]string{}
		if !manager.VisitConnectionContextVersion(startVersion, func(key, value string) bool {
			gotStart[key] = value
			return true
		}) {
			t.Fatal("start-time context version should stay retained while command is active")
		}
		if gotStart["tenant"] != "acme" || gotStart["role"] != "reader" {
			t.Fatalf("start-time context while active = %+v, want tenant=acme role=reader", gotStart)
		}
		return apicommand.Result{Value: "OK"}
	})

	result := eval.Evaluate(context.Background(), client, "PINCTX", nil)
	if result.Value != "OK" {
		t.Fatalf("result = %v, want OK", result.Value)
	}
	if !handlerCalled {
		t.Fatal("handler was not called")
	}

	drained := 0
	manager.DrainCompletedShard(0, func(completed commonobs.CompletedOperation) {
		drained++
		if completed.ContextVersion != startVersion {
			t.Fatalf("completed context version = %d, want start-time version %d", completed.ContextVersion, startVersion)
		}
		gotStart := map[string]string{}
		if !manager.VisitConnectionContextVersion(completed.ContextVersion, func(key, value string) bool {
			gotStart[key] = value
			return true
		}) {
			t.Fatal("start-time context version should remain visitable during drain")
		}
		if gotStart["tenant"] != "acme" || gotStart["role"] != "reader" {
			t.Fatalf("drained start-time context = %+v, want tenant=acme role=reader", gotStart)
		}
	})
	if drained != 1 {
		t.Fatalf("drained operations = %d, want 1", drained)
	}
	if manager.VisitConnectionContextVersion(startVersion, nil) {
		t.Fatal("non-current start-time version should be released after drain")
	}
	gotCurrent := map[string]string{}
	if !manager.VisitConnectionContextVersion(currentVersion, func(key, value string) bool {
		gotCurrent[key] = value
		return true
	}) {
		t.Fatal("current connection context version should remain visitable")
	}
	if gotCurrent["tenant"] != "globex" || gotCurrent["role"] != "reader" {
		t.Fatalf("current context = %+v, want tenant=globex role=reader", gotCurrent)
	}
}

func TestEvaluate_CommandScopePinsConnectionBaseAndCommandOverlay(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	manager := newPipelineTelemetryTracker()
	eval.SetOperationTrackerManager(manager)

	connection := apiobs.ConnectionIdentity(79)
	baseVersion := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	client := clientctx.New()
	client.ConnectionIdentity = connection
	client.OperationID = "parent-cmd-meta"
	client.CmdMeta = map[string]string{"tenant": "globex", "traceparent": "00-abc"}

	result := eval.Evaluate(context.Background(), client, "PING", nil)
	if result.Value != "PONG" {
		t.Fatalf("PING result = %v, want PONG", result.Value)
	}

	drained := drainTelemetryOperations(manager)
	if len(drained) != 1 {
		t.Fatalf("drained operations = %d, want 1", len(drained))
	}
	completed := drained[0]
	if completed.contextVersion != baseVersion {
		t.Fatalf("completed context version = %d, want connection base %d", completed.contextVersion, baseVersion)
	}
	if completed.contextOverlay["tenant"] != "globex" || completed.contextOverlay["traceparent"] != "00-abc" {
		t.Fatalf("completed context overlay = %+v, want tenant=globex traceparent=00-abc", completed.contextOverlay)
	}

	current := make(map[string]string)
	if !manager.VisitConnectionContextVersion(baseVersion, func(key, value string) bool {
		current[key] = value
		return true
	}) {
		t.Fatal("connection base should remain visitable")
	}
	if current["tenant"] != "acme" || current["role"] != "reader" {
		t.Fatalf("connection base = %+v, want tenant=acme role=reader", current)
	}
	if _, ok := current["traceparent"]; ok {
		t.Fatalf("command metadata leaked into connection base: %+v", current)
	}
}

func TestEvaluate_RexMetaSetUpdatesFutureConnectionContextVersion(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	manager := newPipelineTelemetryTracker()
	eval.SetOperationTrackerManager(manager)

	connection := apiobs.ConnectionIdentity(78)
	baseVersion := manager.UpdateConnectionContext(connection)
	client := clientctx.New()
	client.ConnectionIdentity = connection
	client.OperationID = "parent-rex-meta"

	setResult := eval.Evaluate(context.Background(), client, "REX.META", []string{"SET", "tenant", "acme"})
	if setResult.Value != "OK" {
		t.Fatalf("REX.META SET result = %v, want OK (err=%v)", setResult.Value, setResult.Err)
	}
	pingResult := eval.Evaluate(context.Background(), client, "PING", nil)
	if pingResult.Value != "PONG" {
		t.Fatalf("PING result = %v, want PONG", pingResult.Value)
	}

	drained := 0
	var rexContextVersion apiobs.ConnectionContextVersion
	var pingContextVersion apiobs.ConnectionContextVersion
	pingContext := map[string]string{}
	manager.DrainCompletedShard(0, func(completed commonobs.CompletedOperation) {
		drained++
		switch drained {
		case 1:
			rexContextVersion = completed.ContextVersion
			if len(completed.Records) == 0 {
				t.Fatal("REX.META should record command start/finish telemetry context")
			}
		case 2:
			pingContextVersion = completed.ContextVersion
			if !manager.VisitConnectionContextVersion(completed.ContextVersion, func(key, value string) bool {
				pingContext[key] = value
				return true
			}) {
				t.Fatal("PING pinned context version should be visitable during drain")
			}
		default:
			t.Fatalf("unexpected extra completed operation: %+v", completed)
		}
	})
	if drained != 2 {
		t.Fatalf("drained operations = %d, want 2", drained)
	}
	if rexContextVersion != baseVersion {
		t.Fatalf("REX.META context version = %d, want initial base version %d", rexContextVersion, baseVersion)
	}
	if pingContextVersion == baseVersion || pingContextVersion.IsZero() {
		t.Fatalf("PING context version = %d, want non-zero version newer than %d", pingContextVersion, baseVersion)
	}
	if pingContext["tenant"] != "acme" {
		t.Fatalf("PING pinned context = %+v, want tenant=acme", pingContext)
	}
}

func TestEvaluate_CommandScopeMarksDeniedPreHookFailed(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	manager := newPipelineTelemetryTracker()
	eval.SetOperationTrackerManager(manager)
	eval.SetHookExecutor(&mockHookExecutor{
		hasAny: true,
		preResult: &apicommand.PreHookResult{
			Denied:     true,
			DenyReason: "unauthorized",
		},
	})

	client := clientctx.New()
	client.OperationID = "parent-denied"
	result := eval.Evaluate(context.Background(), client, "PING", nil)
	if result.Value == "PONG" {
		t.Fatal("denied command should not reach handler")
	}

	drained := drainTelemetryOperations(manager)
	if len(drained) != 1 {
		t.Fatalf("drained operations = %d, want 1", len(drained))
	}
	completed := drained[0]
	if completed.parent != client.OperationID {
		t.Fatalf("parent = %q, want %q", completed.parent, client.OperationID)
	}
	if completed.status != commonobs.SlotTerminalFailed {
		t.Fatalf("status = %v, want failed", completed.status)
	}
}

func TestEvaluate_WithTracker_CreatesOperation(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	ctx := clientctx.New()
	result := eval.Evaluate(context.Background(), ctx, "PING", nil)
	if result.Value != "PONG" {
		t.Errorf("expected PONG, got %v", result.Value)
	}

	// Operation should be completed and removed from tracker.
	if len(tracker.ActiveOperationSnapshots()) != 0 {
		t.Errorf("expected 0 active operations after command, got %d", len(tracker.ActiveOperationSnapshots()))
	}
}

func TestEvaluate_WithTracker_OperationHasContext(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	// Use a mock op hook executor that captures the operation.
	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	if opHook.startCalled.Load() != 1 {
		t.Errorf("expected start hook called once, got %d", opHook.startCalled.Load())
	}
	if opHook.completeCalled.Load() != 1 {
		t.Errorf("expected complete hook called once, got %d", opHook.completeCalled.Load())
	}

	// Verify the operation had correct context.
	op := opHook.lastOp.Load()
	if op == nil {
		t.Fatal("expected operation to be captured")
	}
	if op.Type != ops.TypeCommand {
		t.Errorf("expected TypeCommand, got %v", op.Type)
	}
	cmdVal, _ := op.Get("_command")
	if cmdVal != "PING" {
		t.Errorf("expected _command=PING, got %q", cmdVal)
	}
}

func TestEvaluate_WithTracker_ParentID(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	ctx := clientctx.New()
	ctx.OperationID = "conn_1" // simulate connection operation

	eval.Evaluate(context.Background(), ctx, "PING", nil)

	if opHook.lastOp.Load().ParentID != "conn_1" {
		t.Errorf("expected parent conn_1, got %q", opHook.lastOp.Load().ParentID)
	}
}

func TestEvaluate_WithTracker_REXMetadataInContext(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	ctx := clientctx.New()
	ctx.CmdMeta = map[string]string{
		"traceparent": "00-abc-def-01",
		"tenant":      "acme",
	}

	eval.Evaluate(context.Background(), ctx, "PING", nil)

	// REX metadata should be in operation context with shared.rex. prefix.
	tp, ok := opHook.lastOp.Load().Get("shared.rex.traceparent")
	if !ok || tp != "00-abc-def-01" {
		t.Errorf("expected shared.rex.traceparent, got %q (ok=%v)", tp, ok)
	}
	tenant, ok := opHook.lastOp.Load().Get("shared.rex.tenant")
	if !ok || tenant != "acme" {
		t.Errorf("expected shared.rex.tenant, got %q (ok=%v)", tenant, ok)
	}
}

func TestEvaluate_PluginCommandProjectsOperationContextAndRedactsSecrets(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	manager := newPipelineTelemetryTracker()
	eval.SetOperationTrackerManager(manager)

	pluginRouter := pluginrouter.NewRouter(eval.CoreCommandNames())
	serverPipe, clientPipe := net.Pipe()
	serverConn := transport.NewConn(serverPipe)
	clientConn := transport.NewConn(clientPipe)
	defer serverConn.Close()
	defer clientConn.Close()

	decls := []*gcpc.CommandDeclV1{{Name: "PLUGINONLY", MinArgs: 1, MaxArgs: 1}}
	if err := pluginRouter.RegisterPlugin("echo", serverConn, decls); err != nil {
		t.Fatal(err)
	}
	eval.SetPluginRouter(pluginRouter)
	go pluginRouter.GetPluginConn("echo").StartReadLoop()

	reqCh := make(chan *gcpc.CommandRequestV1, 1)
	errCh := make(chan error, 1)
	go func() {
		env, err := clientConn.Recv()
		if err != nil {
			errCh <- err
			return
		}
		req := env.GetCommandRequest()
		if req == nil {
			errCh <- context.Canceled
			return
		}
		reqCh <- req
		result := &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: "hello"}}
		if err := clientConn.Send(gcpc.NewCommandResponse(req.RequestId, result, false)); err != nil {
			errCh <- err
		}
	}()

	client := clientctx.New()
	client.ConnectionIdentity = apiobs.ConnectionIdentity(99)
	client.ConnectionID = "cid_99"
	client.RemoteAddr = "127.0.0.1:6379"
	client.OperationID = "conn_99"
	client.CmdMeta = map[string]string{
		"traceparent": "00-abc",
		"tenant":      "acme",
	}
	manager.UpdateConnectionContextStrings(
		client.ConnectionIdentity,
		"shared.user", "alice",
		"shared.secret.jwt", "hidden",
		"echo.private", "visible",
		"echo.secret.token", "hidden",
	)

	res := eval.Evaluate(context.Background(), client, "PLUGINONLY", []string{"hello"})
	if res.Err != nil {
		t.Fatalf("plugin command returned error: %v", res.Err)
	}
	if res.Value != "hello" {
		t.Fatalf("plugin command value = %v, want hello", res.Value)
	}

	select {
	case err := <-errCh:
		t.Fatalf("plugin side error: %v", err)
	default:
	}

	var req *gcpc.CommandRequestV1
	select {
	case req = <-reqCh:
	default:
		t.Fatal("plugin did not receive command request")
	}

	if req.Metadata["traceparent"] != "00-abc" {
		t.Errorf("metadata traceparent = %q, want 00-abc", req.Metadata["traceparent"])
	}
	if req.Metadata["tenant"] != "acme" {
		t.Errorf("metadata tenant = %q, want acme", req.Metadata["tenant"])
	}
	if _, ok := req.Context["traceparent"]; ok {
		t.Errorf("bare metadata key leaked into context: %v", req.Context)
	}
	if req.Context["shared.rex.traceparent"] != "00-abc" {
		t.Errorf("context shared.rex.traceparent = %q, want 00-abc", req.Context["shared.rex.traceparent"])
	}
	if req.Context["shared.rex.tenant"] != "acme" {
		t.Errorf("context shared.rex.tenant = %q, want acme", req.Context["shared.rex.tenant"])
	}
	if req.Context["shared.user"] != "alice" {
		t.Errorf("context shared.user = %q, want alice", req.Context["shared.user"])
	}
	if req.Context["echo.private"] != "visible" {
		t.Errorf("context echo.private = %q, want visible", req.Context["echo.private"])
	}
	if req.Context[apicommand.CommandKey] != "PLUGINONLY" {
		t.Errorf("context command = %q, want PLUGINONLY", req.Context[apicommand.CommandKey])
	}
	if _, ok := req.Context["shared.secret.jwt"]; ok {
		t.Errorf("shared secret leaked into context: %v", req.Context)
	}
	if _, ok := req.Context["echo.secret.token"]; ok {
		t.Errorf("plugin secret leaked into context: %v", req.Context)
	}
}

func TestEvaluate_WithTracker_OpHookEnrichment(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	// Op hook enriches with traceparent during start.
	opHook := &mockOpHookExecutor{
		hasAny: true,
		enrichOnStart: map[string]string{
			"shared.traceparent": "00-generated-abc-01",
			"span_id":            "xyz",
		},
	}
	eval.SetOpHookExecutor(opHook)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	// Verify enrichment landed in operation.
	tp, ok := opHook.lastOp.Load().Get("shared.traceparent")
	if !ok || tp != "00-generated-abc-01" {
		t.Errorf("expected shared.traceparent enrichment, got %q", tp)
	}
}

func TestEvaluate_WithoutSidecarDoesNotEmitCommandEventsDirectly(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	emitter := &mockEmitter{}
	eval.SetEmitter(emitter)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	if len(emitter.events) != 0 {
		t.Fatalf("expected no direct command events without sidecar scope, got %d", len(emitter.events))
	}
}

func TestEvaluate_WithTracker_CmdHooksDeriveFromOpContext(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	// Op hook enriches operation context.
	opHook := &mockOpHookExecutor{
		hasAny:        true,
		enrichOnStart: map[string]string{"shared.traceparent": "00-test-123-01"},
	}
	eval.SetOpHookExecutor(opHook)

	// Command hook should see the enriched context.
	cmdHook := &mockHookExecutor{
		hasAny:    true,
		preResult: &apicommand.PreHookResult{Denied: false},
	}
	eval.SetHookExecutor(cmdHook)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	// The hook context should contain the operation-enriched traceparent.
	if cmdHook.lastHookCtx == nil {
		t.Fatal("expected hook context to be set")
	}
	if cmdHook.lastHookCtx["shared.traceparent"] != "00-test-123-01" {
		t.Errorf("hook context should contain op-enriched traceparent, got %q",
			cmdHook.lastHookCtx["shared.traceparent"])
	}
	// Should also contain server-injected keys.
	if cmdHook.lastHookCtx[apicommand.StartNs] == "" {
		t.Error("hook context should contain _start_ns")
	}
	if cmdHook.lastHookCtx[apicommand.OperationID] == "" {
		t.Error("hook context should contain _operation_id")
	}
}

func TestEvaluate_WithTracker_PreHookDeny_FailsOperation(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	cmdHook := &mockHookExecutor{
		hasAny: true,
		preResult: &apicommand.PreHookResult{
			Denied:     true,
			DenyReason: "unauthorized",
		},
	}
	eval.SetHookExecutor(cmdHook)

	ctx := clientctx.New()
	result := eval.Evaluate(context.Background(), ctx, "PING", nil)

	// Command should be denied.
	if result.Value == "PONG" {
		t.Error("command should have been denied")
	}

	// Operation should have been failed and cleaned up.
	if len(tracker.ActiveOperationSnapshots()) != 0 {
		t.Errorf("expected 0 active after denied command, got %d", len(tracker.ActiveOperationSnapshots()))
	}

	// Complete hook should have fired (for cleanup/observation).
	if opHook.completeCalled.Load() != 1 {
		t.Errorf("expected complete hook called after deny, got %d", opHook.completeCalled.Load())
	}
}

func TestEvaluate_WithTracker_PreHookEnrichmentFlowsToOperation(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	// Pre-hook adds context values.
	cmdHook := &mockHookExecutor{
		hasAny: true,
		preResult: &apicommand.PreHookResult{
			Denied: false,
			Context: map[string]string{
				"_start_ns":      "12345",
				"_operation_id":  "will-be-overwritten",
				"shared.user":    "john",
				"auth.cache_hit": "true",
			},
		},
	}
	eval.SetHookExecutor(cmdHook)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	// Pre-hook enrichments should be in the operation.
	user, _ := opHook.lastOp.Load().Get("shared.user")
	if user != "john" {
		t.Errorf("expected shared.user=john from pre-hook, got %q", user)
	}
}

func TestEvaluate_UnknownCommand(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	ctx := clientctx.New()
	result := eval.Evaluate(context.Background(), ctx, "NOSUCHCMD", nil)

	// Unknown commands don't create operations (they bail before the op lifecycle).
	if len(tracker.ActiveOperationSnapshots()) != 0 {
		t.Errorf("expected 0 active, got %d", len(tracker.ActiveOperationSnapshots()))
	}
	_ = result
}

func TestEvaluate_TransactionQueued(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	ctx := clientctx.New()
	ctx.InTransaction = true

	result := eval.Evaluate(context.Background(), ctx, "SET", []string{"key", "value"})
	if result.Value != "QUEUED" {
		t.Errorf("expected QUEUED, got %v", result.Value)
	}

	// Queued commands don't create operations.
	if len(tracker.ActiveOperationSnapshots()) != 0 {
		t.Errorf("expected 0 active for queued command, got %d", len(tracker.ActiveOperationSnapshots()))
	}
}

func TestEvaluate_ConcurrentCommands(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	done := make(chan struct{})
	const n = 50

	for range n {
		go func() {
			ctx := clientctx.New()
			eval.Evaluate(context.Background(), ctx, "PING", nil)
			done <- struct{}{}
		}()
	}

	for range n {
		<-done
	}

	if len(tracker.ActiveOperationSnapshots()) != 0 {
		t.Errorf("expected 0 active after all commands, got %d", len(tracker.ActiveOperationSnapshots()))
	}

	if opHook.startCalled.Load() != int32(n) {
		t.Errorf("expected %d start calls, got %d", n, opHook.startCalled.Load())
	}
	if opHook.completeCalled.Load() != int32(n) {
		t.Errorf("expected %d complete calls, got %d", n, opHook.completeCalled.Load())
	}
}

func TestEvaluate_OperationTimingAccuracy(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	manager := newPipelineTelemetryTracker()
	eval.SetOperationTrackerManager(manager)
	eval.SetEmitter(&mockEmitter{})

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	operations := drainTelemetryOperations(manager)
	if len(operations) != 1 {
		t.Fatalf("drained operations = %d, want 1", len(operations))
	}
	for _, record := range operations[0].records {
		if record.Kind != apiobs.TelemetryRecordOperationFinish {
			continue
		}
		if record.Number == 0 {
			t.Fatal("expected non-zero elapsed_ns on operation finish record")
		}
		status, ok := telemetryRecordStringField(record, "_status")
		if !ok || status != "completed" {
			t.Fatalf("operation finish status field = %q/%v, want completed/true", status, ok)
		}
		return
	}
	t.Fatal("operation finish record not found")
}

func TestEvaluate_OperationContextHasElapsed(t *testing.T) {
	eval, e, _ := newTestPipeline()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	// The completed operation should have _elapsed_ns in context.
	elapsed, ok := opHook.lastOp.Load().Get(apicommand.ElapsedNs)
	if !ok || elapsed == "" {
		t.Error("expected _elapsed_ns in operation context")
	}
	ns, err := strconv.ParseUint(elapsed, 10, 64)
	if err != nil || ns == 0 {
		t.Errorf("expected valid non-zero elapsed, got %q", elapsed)
	}
}

func TestEvaluate_ArgValidation_NoOperation(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	ctx := clientctx.New()
	// SET requires 2 args.
	result := eval.Evaluate(context.Background(), ctx, "SET", []string{"key"})

	// Arg validation failure happens before operation creation.
	// Actually — arg validation happens BEFORE op creation in current code.
	// That's correct: no operation for invalid commands.
	if len(tracker.ActiveOperationSnapshots()) != 0 {
		t.Errorf("expected 0 active after arg validation failure, got %d", len(tracker.ActiveOperationSnapshots()))
	}
	_ = result
}
