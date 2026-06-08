package pipeline

import (
	"context"
	"testing"

	apiEvents "gocache/api/events"
	apiobs "gocache/api/observability"
	commonobs "gocache/commons/observability"
	"gocache/pkg/benchstats"
	"gocache/pkg/clientctx"
)

func TestEvaluate_TelemetryScopeSaturationSkipsWithoutFakeRecords(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*commonobs.SlotOperationTrackerManager, *clientctx.ClientContext)
	}{
		{
			name: "non-pinned connection context",
			setup: func(manager *commonobs.SlotOperationTrackerManager, client *clientctx.ClientContext) {
				client.ConnectionIdentity = apiobs.ConnectionIdentity(31)
				manager.UpdateConnectionContextStrings(client.ConnectionIdentity, "tenant", "acme")
			},
		},
		{
			name: "pinned owner context with magazine",
			setup: func(manager *commonobs.SlotOperationTrackerManager, client *clientctx.ClientContext) {
				client.ConnectionIdentity = apiobs.ConnectionIdentity(32)
				manager.UpdateOwnedConnectionContextStrings(&client.ConnectionContext, client.ConnectionIdentity, "tenant", "acme")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval, e, _ := newTestPipeline()
			defer e.Stop()

			manager := newPipelineSaturationTracker()
			eval.SetOperationTrackerManager(manager)
			emitter := &mockEmitter{}
			eval.SetEmitter(emitter)

			blocker, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
			if !ok {
				t.Fatal("blocker operation should occupy the only slot")
			}

			client := clientctx.New()
			client.OperationID = "parent-saturated"
			client.CmdMeta = map[string]string{"traceparent": "00-abc"}
			tt.setup(manager, client)

			result := eval.Evaluate(context.Background(), client, "PING", nil)
			if result.Value != "PONG" {
				t.Fatalf("PING result = %v, want PONG", result.Value)
			}
			if skipped := manager.SkippedOperations(); skipped != 1 {
				t.Fatalf("SkippedOperations() = %d, want exactly 1", skipped)
			}
			if len(emitter.events) != 0 {
				t.Fatalf("zero-scope command emitted %d direct events, want 0", len(emitter.events))
			}
			if drained := drainTelemetryOperations(manager); len(drained) != 0 {
				t.Fatalf("zero-scope command materialized %d completed operations, want 0", len(drained))
			}

			if !manager.FinishOperation(blocker, commonobs.SlotTerminalFinished) {
				t.Fatal("blocker finish should enqueue")
			}
			drained := drainTelemetryOperations(manager)
			if len(drained) != 1 {
				t.Fatalf("blocker drain = %d operations, want 1", len(drained))
			}
			if got := countEventRecords(drained[0].records); got != 0 {
				t.Fatalf("saturation blocker event records = %d, want 0", got)
			}
		})
	}
}

func TestEvaluate_TelemetryScopeSaturationDoesNotBuildBenchStartedCompleted(t *testing.T) {
	benchstats.Reset()
	benchstats.SetEnabled(true)
	t.Cleanup(func() {
		benchstats.SetOperationTrackerManager(nil)
		benchstats.Reset()
		benchstats.SetEnabled(false)
	})

	eval, e, _ := newTestPipeline()
	defer e.Stop()

	manager := newPipelineSaturationTracker()
	benchstats.SetOperationTrackerManager(manager)
	eval.SetOperationTrackerManager(manager)
	eval.SetEmitter(&mockEmitter{})

	blocker, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("blocker operation should occupy the only slot")
	}

	client := clientctx.New()
	client.ConnectionIdentity = apiobs.ConnectionIdentity(33)
	for _, eventType := range []apiEvents.Type{apiEvents.OperationStarted, apiEvents.OperationCompleted, apiEvents.CommandStarted, apiEvents.CommandCompleted} {
		if !eval.emitter.HasSubscribersFor(eventType) {
			t.Fatalf("mock emitter should report subscriber for %s", eventType)
		}
	}
	result := eval.Evaluate(context.Background(), client, "PING", nil)
	if result.Value != "PONG" {
		t.Fatalf("PING result = %v, want PONG", result.Value)
	}
	if skipped := manager.SkippedOperations(); skipped != 1 {
		t.Fatalf("SkippedOperations() = %d, want exactly 1", skipped)
	}
	if drained := drainTelemetryOperations(manager); len(drained) != 0 {
		t.Fatalf("zero-scope command materialized %d completed operations, want 0", len(drained))
	}
	stats := benchstats.Snapshot(false)
	checks := map[string]string{
		"pipeline.evaluations":                 "1",
		"pipeline.path.full":                   "1",
		"pipeline.event.operation_started":     "0",
		"pipeline.event.operation_completed":   "0",
		"pipeline.event.command_started":       "0",
		"pipeline.event.command_completed":     "0",
		"operation_tracker.skipped_operations": "1",
		"operation_tracker.dropped_records":    "0",
		"operation_tracker.dropped_completed":  "0",
	}
	for key, want := range checks {
		if got := stats[key]; got != want {
			t.Fatalf("benchstats[%q] = %q, want %q", key, got, want)
		}
	}

	if !manager.FinishOperation(blocker, commonobs.SlotTerminalFinished) {
		t.Fatal("blocker finish should enqueue")
	}
}

func TestEvaluate_TelemetryScopeRecordsOperationLifecycleWithoutEventSubscribers(t *testing.T) {
	benchstats.Reset()
	benchstats.SetEnabled(true)
	t.Cleanup(func() {
		benchstats.SetOperationTrackerManager(nil)
		benchstats.Reset()
		benchstats.SetEnabled(false)
	})

	eval, e, manager := newTestPipeline()
	defer e.Stop()
	benchstats.SetOperationTrackerManager(manager)
	eval.SetEmitter(noSubscriberEmitter{})

	client := clientctx.New()
	client.ConnectionIdentity = apiobs.ConnectionIdentity(34)
	result := eval.Evaluate(context.Background(), client, "PING", nil)
	if result.Value != "PONG" {
		t.Fatalf("PING result = %v, want PONG", result.Value)
	}

	drained := drainTelemetryOperations(manager)
	if len(drained) != 1 {
		t.Fatalf("drained telemetry operations = %d, want 1", len(drained))
	}
	if got := countEventRecords(drained[0].records); got != 2 {
		t.Fatalf("event record count = %d, want operation start and finish", got)
	}
	stats := benchstats.Snapshot(false)
	checks := map[string]string{
		"pipeline.evaluations":               "1",
		"pipeline.event.operation_started":   "1",
		"pipeline.event.operation_completed": "1",
		"pipeline.event.command_started":     "0",
		"pipeline.event.command_completed":   "0",
	}
	for key, want := range checks {
		if got := stats[key]; got != want {
			t.Fatalf("benchstats[%q] = %q, want %q", key, got, want)
		}
	}
}

type noSubscriberEmitter struct{}

func (noSubscriberEmitter) Emit(apiEvents.Event)                     {}
func (noSubscriberEmitter) HasSubscribers() bool                     { return true }
func (noSubscriberEmitter) HasSubscribersFor(...apiEvents.Type) bool { return false }

func newPipelineSaturationTracker() *commonobs.SlotOperationTrackerManager {
	return commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   8,
		CompletedRingPerShard: 1,
	})
}
