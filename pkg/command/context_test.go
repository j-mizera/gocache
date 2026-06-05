package command

import (
	"context"
	"strconv"
	"testing"

	apicommand "gocache/api/command"
	apiobs "gocache/api/observability"
	apipersistence "gocache/api/persistence"
	commonobs "gocache/commons/observability"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/transaction"
	"gocache/pkg/watch"
)

// TestContext_Reset verifies that every field is zeroed so the value can
// be recycled through a sync.Pool without dragging stale references along.
// New fields added to *Context must be reset here too — otherwise the pool
// silently leaks pointers across calls.
func TestContext_Reset(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	ctx := &Context{
		Client:           clientctx.New(),
		Op:               "SET",
		Args:             []string{"k", "v"},
		InBatch:          true,
		ShardLocked:      true,
		Engine:           e,
		Cache:            c,
		Transaction:      transaction.NewManager(),
		BlockingRegistry: blocking.NewRegistry(),
		WatchManager:     watch.NewManager(),
		RequirePass:      "secret",
		Shard:            3,
		MultiKey:         true,
		TouchedShards:    []int{0, 3},
		EvalFn: func(_ context.Context, _ *clientctx.ClientContext, _ string, _ []string, _ bool) apicommand.Result {
			return apicommand.Result{}
		},
		Spec:        apicommand.Spec{ReadOnly: true, KeyArgIndex: 1},
		Coordinator: testMutationEmitter{},
	}
	type testKey struct{}
	ctx.SetContext(context.WithValue(context.Background(), testKey{}, "x"))
	scope, _ := newTestOperationScope(t, 54)
	ctx.SetTelemetry(scope)

	ctx.Reset()

	zeros := []struct {
		name string
		zero bool
	}{
		{"ctx", ctx.Context() == context.Background()}, // Context() returns Background when nil
		{"Client", ctx.Client == nil},
		{"Op", ctx.Op == ""},
		{"Args", ctx.Args == nil},
		{"InBatch", ctx.InBatch == false},
		{"ShardLocked", ctx.ShardLocked == false},
		{"Engine", ctx.Engine == nil},
		{"Cache", ctx.Cache == nil},
		{"Transaction", ctx.Transaction == nil},
		{"BlockingRegistry", ctx.BlockingRegistry == nil},
		{"WatchManager", ctx.WatchManager == nil},
		{"RequirePass", ctx.RequirePass == ""},
		{"Shard", ctx.Shard == 0},
		{"MultiKey", ctx.MultiKey == false},
		{"TouchedShards", ctx.TouchedShards == nil},
		{"EvalFn", ctx.EvalFn == nil},
		{"Spec", ctx.Spec == apicommand.Spec{}},
		{"Coordinator", ctx.Coordinator == nil},
		{"Telemetry", ctx.Telemetry().IsZero()},
	}
	for _, f := range zeros {
		if !f.zero {
			t.Errorf("Reset left %s non-zero", f.name)
		}
	}
}

func TestContext_CancellationContextAccessors(t *testing.T) {
	var ctx Context
	if got := ctx.CancellationContext(); got == nil {
		t.Fatal("CancellationContext returned nil for zero value")
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx.SetCancellationContext(cancelCtx)
	if got := ctx.CancellationContext(); got != cancelCtx {
		t.Fatal("CancellationContext did not return assigned context")
	}
	if got := ctx.Context(); got != cancelCtx {
		t.Fatal("Context compatibility accessor did not return cancellation context")
	}

	aliasCtx, aliasCancel := context.WithCancel(context.Background())
	t.Cleanup(aliasCancel)
	ctx.SetContext(aliasCtx)
	if got := ctx.CancellationContext(); got != aliasCtx {
		t.Fatal("SetContext compatibility mutator did not update cancellation context")
	}
}

func TestContext_TelemetryScopeRecordsAndResets(t *testing.T) {
	var ctx Context
	if ctx.RecordTelemetry(apiobs.NewLogRecordBytes(55, apiobs.TelemetryLogLevelInfo, []byte("zero"))) {
		t.Fatal("zero telemetry scope should reject records")
	}
	if ctx.Log(apiobs.TelemetryLogLevelInfo, []byte("zero")) {
		t.Fatal("zero telemetry scope should reject logs")
	}

	scope, manager := newTestOperationScope(t, 55)
	ctx.SetTelemetry(scope)
	if got := ctx.Telemetry(); got.IsZero() || got.Operation() != 55 || got.Ref().ID.String() != "operation-55" {
		t.Fatalf("Telemetry() = %+v, want active operation scope", got)
	}

	record := apiobs.NewLogRecordBytes(0, apiobs.TelemetryLogLevelWarn, []byte("recorded"))
	if !ctx.RecordTelemetry(record) {
		t.Fatal("RecordTelemetry should accept record with active scope")
	}
	if !ctx.Log(apiobs.TelemetryLogLevelInfo, []byte("logged")) {
		t.Fatal("Log should accept record with active scope")
	}
	if !scope.Finish(commonobs.SlotTerminalFinished) {
		t.Fatal("scope should finish")
	}

	var completed commonobs.CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation commonobs.CompletedOperation) {
		completed = operation
		completed.Records = append([]apiobs.TelemetryRecord(nil), operation.Records...)
	}); drained != 1 {
		t.Fatalf("drained %d operations, want 1", drained)
	}
	if len(completed.Records) != 2 {
		t.Fatalf("record count = %d, want 2", len(completed.Records))
	}
	for i, record := range completed.Records {
		if record.Operation != 55 {
			t.Fatalf("record[%d].Operation = %d, want 55", i, record.Operation)
		}
	}
	if string(completed.Records[0].NameBytes()) != "recorded" || completed.Records[0].Level != apiobs.TelemetryLogLevelWarn {
		t.Fatalf("record[0] = %q/%v, want recorded/warn", completed.Records[0].NameBytes(), completed.Records[0].Level)
	}
	if string(completed.Records[1].NameBytes()) != "logged" || completed.Records[1].Level != apiobs.TelemetryLogLevelInfo {
		t.Fatalf("record[1] = %q/%v, want logged/info", completed.Records[1].NameBytes(), completed.Records[1].Level)
	}

	ctx.Reset()
	if !ctx.Telemetry().IsZero() {
		t.Fatal("Reset should clear telemetry scope")
	}
}

func newTestOperationScope(t *testing.T, operation apiobs.InternalOperationIdentity) (commonobs.OperationScope, *commonobs.SlotOperationTrackerManager) {
	t.Helper()
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   2,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(operation, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate command slot")
	}
	ref := apiobs.NewOperationRef("operation-"+strconv.FormatUint(uint64(operation), 10), "")
	return commonobs.NewOperationScope(manager, handle, operation, ref), manager
}

type testMutationEmitter struct{}

func (testMutationEmitter) HasSinks() bool { return false }

func (testMutationEmitter) AllocateAndEmit(string, string, [][]byte) apipersistence.LSN {
	return 0
}
