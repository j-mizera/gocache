package pipeline

import (
	"context"
	"testing"

	apiEvents "gocache/api/events"
	"gocache/pkg/clientctx"
	"gocache/pkg/events"
)

type mockCommandMetricsRecorder struct {
	active    bool
	commands  []string
	elapsedNs []uint64
	errors    []bool
}

func (m *mockCommandMetricsRecorder) HasCommandMetricsSink() bool { return m.active }

func (m *mockCommandMetricsRecorder) RecordCommand(command string, elapsedNs uint64, isError bool) {
	m.commands = append(m.commands, command)
	m.elapsedNs = append(m.elapsedNs, elapsedNs)
	m.errors = append(m.errors, isError)
}

func TestNoSinksStillCaptureCommandTelemetry(t *testing.T) {
	eval, e, manager := newTestPipeline()
	defer e.Stop()

	const n = 25
	ctx := clientctx.New()
	for i := 0; i < n; i++ {
		res := eval.Evaluate(context.Background(), ctx, "PING", nil)
		if res.Value != "PONG" {
			t.Fatalf("command %d: want PONG, got %v", i, res.Value)
		}
	}

	if got := len(manager.ActiveOperationSnapshots()); got != 0 {
		t.Errorf("ActiveCount after commands: want 0, got %d", got)
	}
	if got := manager.SkippedOperations(); got != 0 {
		t.Errorf("SkippedCount after no-sink commands: want 0, got %d", got)
	}
	if drained := drainTelemetryOperations(manager); len(drained) != n {
		t.Fatalf("drained telemetry operations = %d, want %d", len(drained), n)
	}
}

func TestCommandEventSubscribersStillGateOnlyFanout(t *testing.T) {
	eval, e, manager := newTestPipeline()
	defer e.Stop()

	bus := events.NewBus()
	eval.SetEmitter(bus)

	ctx := clientctx.New()
	for i := 0; i < 4; i++ {
		eval.Evaluate(context.Background(), ctx, "PING", nil)
	}
	drainTelemetryOperations(manager)

	captured := 0
	bus.Subscribe("test", []apiEvents.Type{
		apiEvents.OperationStarted,
		apiEvents.OperationCompleted,
		apiEvents.CommandStarted,
		apiEvents.CommandCompleted,
	}, func(apiEvents.Event) { captured++ })

	const afterN = 3
	for i := 0; i < afterN; i++ {
		eval.Evaluate(context.Background(), ctx, "PING", nil)
	}

	if captured != 0 {
		t.Errorf("command path emitted inline events: got %d, want 0 before sidecar drain", captured)
	}
	if got := manager.SkippedOperations(); got != 0 {
		t.Errorf("SkippedCount must not advance with or without subscribers: want 0, got %d", got)
	}
	drained := drainTelemetryOperations(manager)
	if len(drained) != afterN {
		t.Fatalf("drained telemetry operations = %d, want %d", len(drained), afterN)
	}
	for _, operation := range drained {
		if got := countEventRecords(operation.records); got != 4 {
			t.Fatalf("sidecar event records = %d, want 4", got)
		}
	}
}

func TestLogOnlySubscriberDoesNotSuppressTelemetry(t *testing.T) {
	eval, e, manager := newTestPipeline()
	defer e.Stop()

	bus := events.NewBus()
	eval.SetEmitter(bus)
	bus.Subscribe("logs", []apiEvents.Type{apiEvents.RuntimeLogBatch}, func(apiEvents.Event) {})

	ctx := clientctx.New()
	res := eval.Evaluate(context.Background(), ctx, "PING", nil)
	if res.Value != "PONG" {
		t.Fatalf("want PONG, got %v", res.Value)
	}
	if got := manager.SkippedOperations(); got != 0 {
		t.Fatalf("log-only subscriber should not cause telemetry skip; skipped=%d", got)
	}
	if drained := drainTelemetryOperations(manager); len(drained) != 1 {
		t.Fatalf("drained telemetry operations = %d, want 1", len(drained))
	}
}

func TestCommandMetricsSinkRecordsMetricsWithoutSuppressingTelemetry(t *testing.T) {
	eval, e, manager := newTestPipeline()
	defer e.Stop()

	metrics := &mockCommandMetricsRecorder{active: true}
	eval.SetCommandMetricsRecorder(metrics)

	ctx := clientctx.New()
	res := eval.Evaluate(context.Background(), ctx, "PING", nil)
	if res.Value != "PONG" {
		t.Fatalf("want PONG, got %v", res.Value)
	}
	if got := manager.SkippedOperations(); got != 0 {
		t.Fatalf("metrics sink should not suppress telemetry; skipped=%d", got)
	}
	if len(metrics.commands) != 1 {
		t.Fatalf("recorded commands=%v, want one", metrics.commands)
	}
	if metrics.commands[0] != "PING" {
		t.Fatalf("recorded command=%q, want PING", metrics.commands[0])
	}
	if metrics.elapsedNs[0] == 0 {
		t.Fatal("expected non-zero elapsed time")
	}
	if metrics.errors[0] {
		t.Fatal("PING should not record an error")
	}
	if drained := drainTelemetryOperations(manager); len(drained) != 1 {
		t.Fatalf("drained telemetry operations = %d, want 1", len(drained))
	}
}
