package evaluator

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	apiEvents "gocache/api/events"
	ops "gocache/api/operations"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/watch"
)

// --- Test helpers ---

func newTestEvaluator() (*BaseEvaluator, *engine.Engine, *serverOps.Tracker) {
	c := cache.New()
	e := engine.New(c)
	go e.Run()
	br := blocking.NewRegistry()
	wm := watch.NewManager()
	eval := New(c, e, "test.dat", "", br, wm)
	tracker := serverOps.NewTracker()
	eval.SetTracker(tracker)
	return eval, e, tracker
}

// mockHookExecutor implements command.HookExecutor for testing.
type mockHookExecutor struct {
	hasAny      bool
	preResult   *command.PreHookResult
	postCalled  atomic.Int32
	lastHookCtx map[string]string
}

func (m *mockHookExecutor) HasAny() bool { return m.hasAny }
func (m *mockHookExecutor) RunPreHooks(_ context.Context, _ string, _ []string, hookCtx map[string]string) *command.PreHookResult {
	m.lastHookCtx = hookCtx
	return m.preResult
}
func (m *mockHookExecutor) RunPostHooks(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) {
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

func (m *mockOpHookExecutor) HasAny() bool { return m.hasAny }
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

// mockEmitter collects emitted events.
type mockEmitter struct {
	events []apiEvents.Event
}

func (m *mockEmitter) Emit(evt apiEvents.Event) {
	m.events = append(m.events, evt)
}

// --- Tests ---

func TestEvaluate_BasicCommand(t *testing.T) {
	eval, e, _ := newTestEvaluator()
	defer e.Stop()

	ctx := clientctx.New()
	result := eval.Evaluate(context.Background(), ctx, "PING", nil)
	if result.Value != "PONG" {
		t.Errorf("expected PONG, got %v", result.Value)
	}
}

func TestEvaluate_WithTracker_CreatesOperation(t *testing.T) {
	eval, e, tracker := newTestEvaluator()
	defer e.Stop()

	ctx := clientctx.New()
	result := eval.Evaluate(context.Background(), ctx, "PING", nil)
	if result.Value != "PONG" {
		t.Errorf("expected PONG, got %v", result.Value)
	}

	// Operation should be completed and removed from tracker.
	if tracker.ActiveCount() != 0 {
		t.Errorf("expected 0 active operations after command, got %d", tracker.ActiveCount())
	}
}

func TestEvaluate_WithTracker_OperationHasContext(t *testing.T) {
	eval, e, _ := newTestEvaluator()
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
	eval, e, _ := newTestEvaluator()
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
	eval, e, _ := newTestEvaluator()
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

func TestEvaluate_WithTracker_OpHookEnrichment(t *testing.T) {
	eval, e, _ := newTestEvaluator()
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

func TestEvaluate_WithTracker_EmitsEvents(t *testing.T) {
	eval, e, _ := newTestEvaluator()
	defer e.Stop()

	emitter := &mockEmitter{}
	eval.SetEmitter(emitter)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	// Should have: operation.start, command.pre, command.post, operation.complete
	if len(emitter.events) < 4 {
		t.Fatalf("expected at least 4 events, got %d", len(emitter.events))
	}

	types := make([]string, len(emitter.events))
	for i, evt := range emitter.events {
		types[i] = evt.Proto.Type
	}

	// Verify order.
	expectedOrder := []string{
		string(apiEvents.OperationStart),
		string(apiEvents.CommandPre),
		string(apiEvents.CommandPost),
		string(apiEvents.OperationComplete),
	}
	for i, expected := range expectedOrder {
		if i >= len(types) {
			t.Errorf("missing event at index %d: expected %s", i, expected)
			continue
		}
		if types[i] != expected {
			t.Errorf("event[%d]: expected %s, got %s", i, expected, types[i])
		}
	}

	// All events should carry the operation_id.
	opID := emitter.events[0].Proto.OperationId
	if opID == "" {
		t.Fatal("expected non-empty operation_id on first event")
	}
	for i, evt := range emitter.events {
		if evt.Proto.OperationId != opID {
			t.Errorf("event[%d] operation_id mismatch: %q vs %q", i, evt.Proto.OperationId, opID)
		}
	}
}

func TestEvaluate_WithTracker_CmdHooksDeriveFromOpContext(t *testing.T) {
	eval, e, _ := newTestEvaluator()
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
		preResult: &command.PreHookResult{Denied: false},
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
	if cmdHook.lastHookCtx[command.StartNs] == "" {
		t.Error("hook context should contain _start_ns")
	}
	if cmdHook.lastHookCtx[command.OperationID] == "" {
		t.Error("hook context should contain _operation_id")
	}
}

func TestEvaluate_WithTracker_PreHookDeny_FailsOperation(t *testing.T) {
	eval, e, tracker := newTestEvaluator()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	cmdHook := &mockHookExecutor{
		hasAny: true,
		preResult: &command.PreHookResult{
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
	if tracker.ActiveCount() != 0 {
		t.Errorf("expected 0 active after denied command, got %d", tracker.ActiveCount())
	}

	// Complete hook should have fired (for cleanup/observation).
	if opHook.completeCalled.Load() != 1 {
		t.Errorf("expected complete hook called after deny, got %d", opHook.completeCalled.Load())
	}
}

func TestEvaluate_WithTracker_PreHookEnrichmentFlowsToOperation(t *testing.T) {
	eval, e, _ := newTestEvaluator()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	// Pre-hook adds context values.
	cmdHook := &mockHookExecutor{
		hasAny: true,
		preResult: &command.PreHookResult{
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
	eval, e, tracker := newTestEvaluator()
	defer e.Stop()

	ctx := clientctx.New()
	result := eval.Evaluate(context.Background(), ctx, "NOSUCHCMD", nil)

	// Unknown commands don't create operations (they bail before the op lifecycle).
	if tracker.ActiveCount() != 0 {
		t.Errorf("expected 0 active, got %d", tracker.ActiveCount())
	}
	_ = result
}

func TestEvaluate_TransactionQueued(t *testing.T) {
	eval, e, tracker := newTestEvaluator()
	defer e.Stop()

	ctx := clientctx.New()
	ctx.InTransaction = true

	result := eval.Evaluate(context.Background(), ctx, "SET", []string{"key", "value"})
	if result.Value != "QUEUED" {
		t.Errorf("expected QUEUED, got %v", result.Value)
	}

	// Queued commands don't create operations.
	if tracker.ActiveCount() != 0 {
		t.Errorf("expected 0 active for queued command, got %d", tracker.ActiveCount())
	}
}

func TestEvaluate_ConcurrentCommands(t *testing.T) {
	eval, e, tracker := newTestEvaluator()
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

	if tracker.ActiveCount() != 0 {
		t.Errorf("expected 0 active after all commands, got %d", tracker.ActiveCount())
	}

	if opHook.startCalled.Load() != int32(n) {
		t.Errorf("expected %d start calls, got %d", n, opHook.startCalled.Load())
	}
	if opHook.completeCalled.Load() != int32(n) {
		t.Errorf("expected %d complete calls, got %d", n, opHook.completeCalled.Load())
	}
}

func TestEvaluate_OperationTimingAccuracy(t *testing.T) {
	eval, e, _ := newTestEvaluator()
	defer e.Stop()

	emitter := &mockEmitter{}
	eval.SetEmitter(emitter)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	// Find the operation.complete event and check elapsed_ns > 0.
	for _, evt := range emitter.events {
		if evt.Proto.Type == string(apiEvents.OperationComplete) {
			data := evt.Proto.GetOperationComplete()
			if data == nil {
				t.Fatal("expected OperationCompleteEventV1")
			}
			if data.ElapsedNs == 0 {
				t.Error("expected non-zero elapsed_ns")
			}
			if data.Status != "completed" {
				t.Errorf("expected completed, got %s", data.Status)
			}
			return
		}
	}
	t.Error("operation.complete event not found")
}

func TestEvaluate_OperationContextHasElapsed(t *testing.T) {
	eval, e, _ := newTestEvaluator()
	defer e.Stop()

	opHook := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(opHook)

	ctx := clientctx.New()
	eval.Evaluate(context.Background(), ctx, "PING", nil)

	// The completed operation should have _elapsed_ns in context.
	elapsed, ok := opHook.lastOp.Load().Get(command.ElapsedNs)
	if !ok || elapsed == "" {
		t.Error("expected _elapsed_ns in operation context")
	}
	ns, err := strconv.ParseUint(elapsed, 10, 64)
	if err != nil || ns == 0 {
		t.Errorf("expected valid non-zero elapsed, got %q", elapsed)
	}
}

func TestEvaluate_ArgValidation_NoOperation(t *testing.T) {
	eval, e, tracker := newTestEvaluator()
	defer e.Stop()

	ctx := clientctx.New()
	// SET requires 2 args.
	result := eval.Evaluate(context.Background(), ctx, "SET", []string{"key"})

	// Arg validation failure happens before operation creation.
	// Actually — arg validation happens BEFORE op creation in current code.
	// That's correct: no operation for invalid commands.
	if tracker.ActiveCount() != 0 {
		t.Errorf("expected 0 active after arg validation failure, got %d", tracker.ActiveCount())
	}
	_ = result
}
