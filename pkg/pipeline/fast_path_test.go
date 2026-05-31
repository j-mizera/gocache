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

// TestFastPath_NoSinks_BypassesTracker confirms that with no emitter,
// command-hook executor, or op-hook executor wired, the evaluator skips
// the tracker entirely. ActiveCount stays zero and SkippedCount climbs by
// the number of commands run.
func TestFastPath_NoSinks_BypassesTracker(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	if got := tracker.SkippedCount(); got != 0 {
		t.Fatalf("baseline SkippedCount: want 0, got %d", got)
	}

	const n = 25
	ctx := clientctx.New()
	for i := 0; i < n; i++ {
		res := eval.Evaluate(context.Background(), ctx, "PING", nil)
		if res.Value != "PONG" {
			t.Fatalf("command %d: want PONG, got %v", i, res.Value)
		}
	}

	if got := tracker.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount after fast-path runs: want 0, got %d", got)
	}
	if got := tracker.SkippedCount(); got != n {
		t.Errorf("SkippedCount: want %d, got %d", n, got)
	}
}

// TestFastPath_BusEmitter_RoutesToSlowPath confirms that attaching a real
// bus with a subscriber routes new commands through the instrumented path
// (events fire, tracker registers and unregisters) — even though the
// fast-path branch is also present.
func TestFastPath_BusEmitter_RoutesToSlowPath(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	emitter := &mockEmitter{}
	eval.SetEmitter(emitter)

	const n = 5
	ctx := clientctx.New()
	for i := 0; i < n; i++ {
		eval.Evaluate(context.Background(), ctx, "PING", nil)
	}

	// Slow path emits 4 events per command (op.start, command.pre,
	// command.completed, op.completed) → 4n events total.
	if want, got := 4*n, len(emitter.events); want != got {
		t.Errorf("events captured: want %d, got %d", want, got)
	}
	if got := tracker.SkippedCount(); got != 0 {
		t.Errorf("SkippedCount under slow path: want 0, got %d", got)
	}
}

// TestFastPath_HookExecutor_RoutesToSlowPath confirms that attaching a
// command-hook executor with HasAny=true routes new commands through the
// slow path so pre-hooks fire.
func TestFastPath_HookExecutor_RoutesToSlowPath(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	hooks := &mockHookExecutor{hasAny: true}
	eval.SetHookExecutor(hooks)

	const n = 7
	ctx := clientctx.New()
	for i := 0; i < n; i++ {
		eval.Evaluate(context.Background(), ctx, "PING", nil)
	}

	if got := hooks.postCalled.Load(); int(got) != n {
		t.Errorf("post-hooks fired: want %d, got %d", n, got)
	}
	if got := tracker.SkippedCount(); got != 0 {
		t.Errorf("SkippedCount under slow path: want 0, got %d", got)
	}
}

// TestFastPath_OpHookExecutor_RoutesToSlowPath confirms that attaching an
// operation-hook executor with HasAny=true routes new commands through the
// slow path so start/complete ophooks fire.
func TestFastPath_OpHookExecutor_RoutesToSlowPath(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	ophooks := &mockOpHookExecutor{hasAny: true}
	eval.SetOpHookExecutor(ophooks)

	const n = 3
	ctx := clientctx.New()
	for i := 0; i < n; i++ {
		eval.Evaluate(context.Background(), ctx, "PING", nil)
	}

	if got := ophooks.startCalled.Load(); int(got) != n {
		t.Errorf("start ophooks fired: want %d, got %d", n, got)
	}
	if got := ophooks.completeCalled.Load(); int(got) != n {
		t.Errorf("complete ophooks fired: want %d, got %d", n, got)
	}
	if got := tracker.SkippedCount(); got != 0 {
		t.Errorf("SkippedCount under slow path: want 0, got %d", got)
	}
}

// TestFastPath_MidStreamSubscribe verifies the atomic counter flip is
// observed without a memory-barrier bug: commands before subscribe go
// through the fast path; commands after subscribe go through the slow path.
//
// HasSubscribers/HasAny on the wired sinks (real *events.Bus,
// *cmdhooks.Registry, *ophooks.Registry) read atomic counters that are
// updated under the same write lock that mutates the underlying maps. So
// once Subscribe / Register returns, all subsequent evaluator calls see
// the new state.
func TestFastPath_LogOnlySubscriberKeepsCommandFastPath(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	bus := events.NewBus()
	eval.SetEmitter(bus)
	bus.Subscribe("logs", []apiEvents.Type{apiEvents.RuntimeLogBatch}, func(apiEvents.Event) {})

	ctx := clientctx.New()
	res := eval.Evaluate(context.Background(), ctx, "PING", nil)
	if res.Value != "PONG" {
		t.Fatalf("want PONG, got %v", res.Value)
	}
	if got := tracker.SkippedCount(); got != 1 {
		t.Fatalf("log-only subscriber should keep command on fast path; skipped=%d", got)
	}
}

func TestFastPath_CommandMetricsSinkUsesMetricsOnlyPath(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	metrics := &mockCommandMetricsRecorder{active: true}
	eval.SetCommandMetricsRecorder(metrics)

	ctx := clientctx.New()
	res := eval.Evaluate(context.Background(), ctx, "PING", nil)
	if res.Value != "PONG" {
		t.Fatalf("want PONG, got %v", res.Value)
	}
	if got := tracker.SkippedCount(); got != 1 {
		t.Fatalf("metrics-only path should still skip tracker registration; skipped=%d", got)
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
}

func TestFastPath_MidStreamSubscribe(t *testing.T) {
	eval, e, tracker := newTestPipeline()
	defer e.Stop()

	// Wire a real Bus so we can subscribe mid-stream.
	bus := events.NewBus()
	eval.SetEmitter(bus)

	const beforeN = 4
	ctx := clientctx.New()
	for i := 0; i < beforeN; i++ {
		eval.Evaluate(context.Background(), ctx, "PING", nil)
	}

	if got := tracker.SkippedCount(); got != beforeN {
		t.Errorf("pre-subscribe SkippedCount: want %d, got %d", beforeN, got)
	}

	// Attach a subscriber. From now on every command must take the slow path.
	var captured int
	bus.Subscribe("test", []apiEvents.Type{
		apiEvents.OperationStarted, apiEvents.OperationCompleted,
		apiEvents.CommandStarted, apiEvents.CommandCompleted,
	}, func(_ apiEvents.Event) { captured++ })

	const afterN = 3
	for i := 0; i < afterN; i++ {
		eval.Evaluate(context.Background(), ctx, "PING", nil)
	}

	if want := 4 * afterN; captured != want {
		t.Errorf("post-subscribe captured events: want %d, got %d", want, captured)
	}
	if got := tracker.SkippedCount(); got != beforeN {
		t.Errorf("SkippedCount must not advance under slow path: want %d, got %d", beforeN, got)
	}
}
