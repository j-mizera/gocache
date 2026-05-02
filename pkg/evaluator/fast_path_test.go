package evaluator

import (
	"context"
	"testing"

	apiEvents "gocache/api/events"
	"gocache/pkg/clientctx"
	"gocache/pkg/events"
)

// TestFastPath_NoSinks_BypassesTracker confirms that with no emitter,
// command-hook executor, or op-hook executor wired, the evaluator skips
// the tracker entirely. ActiveCount stays zero and SkippedCount climbs by
// the number of commands run.
func TestFastPath_NoSinks_BypassesTracker(t *testing.T) {
	eval, e, tracker := newTestEvaluator()
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
	eval, e, tracker := newTestEvaluator()
	defer e.Stop()

	emitter := &mockEmitter{}
	eval.SetEmitter(emitter)

	const n = 5
	ctx := clientctx.New()
	for i := 0; i < n; i++ {
		eval.Evaluate(context.Background(), ctx, "PING", nil)
	}

	// Slow path emits 4 events per command (op.start, command.pre,
	// command.post, op.complete) → 4n events total.
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
	eval, e, tracker := newTestEvaluator()
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
	eval, e, tracker := newTestEvaluator()
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
func TestFastPath_MidStreamSubscribe(t *testing.T) {
	eval, e, tracker := newTestEvaluator()
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
		apiEvents.OperationStart, apiEvents.OperationComplete,
		apiEvents.CommandPre, apiEvents.CommandPost,
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
