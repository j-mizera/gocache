package ophooks

import (
	"net"
	"testing"
	"time"

	gcpc "gocache/api/gcpc/v1"
	ops "gocache/api/operations"
	"gocache/api/transport"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/plugin/router"
)

func testPipe() (*transport.Conn, *transport.Conn) {
	server, client := net.Pipe()
	return transport.NewConn(server), transport.NewConn(client)
}

func TestRegistry_RegisterAndMatch(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("gobservability", s)
	defer pc.Close()

	reg.Register("gobservability", 10, pc, []string{"*"})

	if !reg.HasAny() {
		t.Error("expected HasAny=true")
	}

	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].PluginName != "gobservability" {
		t.Errorf("expected gobservability, got %s", matches[0].PluginName)
	}
}

func TestRegistry_MatchByType(t *testing.T) {
	reg := NewRegistry()
	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()
	s2, c2 := testPipe()
	defer c2.Close()
	defer s2.Close()

	pc1 := router.NewPluginConn("cmdonly", s1)
	defer pc1.Close()
	pc2 := router.NewPluginConn("all", s2)
	defer pc2.Close()

	reg.Register("cmdonly", 5, pc1, []string{"command"})
	reg.Register("all", 10, pc2, []string{"*"})

	// Command matches both
	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches for command, got %d", len(matches))
	}

	// Cleanup matches only wildcard
	matches = reg.Match(ops.TypeCleanup)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match for cleanup, got %d", len(matches))
	}
	if matches[0].PluginName != "all" {
		t.Errorf("expected 'all', got %s", matches[0].PluginName)
	}
}

func TestRegistry_PriorityOrder(t *testing.T) {
	reg := NewRegistry()
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

	reg.Register("low", 100, pc1, []string{"*"})
	reg.Register("high", 1, pc2, []string{"*"})
	reg.Register("mid", 50, pc3, []string{"*"})

	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 3 {
		t.Fatalf("expected 3, got %d", len(matches))
	}
	if matches[0].PluginName != "high" || matches[1].PluginName != "mid" || matches[2].PluginName != "low" {
		t.Errorf("expected priority order high,mid,low — got %s,%s,%s",
			matches[0].PluginName, matches[1].PluginName, matches[2].PluginName)
	}
}

func TestRegistry_Unregister(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("test", s)
	defer pc.Close()

	reg.Register("test", 10, pc, []string{"*"})
	if !reg.HasAny() {
		t.Fatal("expected hooks registered")
	}

	reg.Unregister("test")
	if reg.HasAny() {
		t.Error("expected no hooks after unregister")
	}
}

func TestRegistry_CaseInsensitive(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("test", s)
	defer pc.Close()

	reg.Register("test", 10, pc, []string{"Command"})

	// Match should be case-insensitive
	matches := reg.Match(ops.TypeCommand) // "command"
	if len(matches) != 1 {
		t.Errorf("expected case-insensitive match, got %d", len(matches))
	}
}

func TestRegistry_NoMatch(t *testing.T) {
	reg := NewRegistry()
	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("test", s)
	defer pc.Close()

	reg.Register("test", 10, pc, []string{"snapshot"})

	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 0 {
		t.Errorf("expected no matches, got %d", len(matches))
	}
}

func TestRegistry_Empty(t *testing.T) {
	reg := NewRegistry()
	if reg.HasAny() {
		t.Error("expected empty registry")
	}
	matches := reg.Match(ops.TypeCommand)
	if len(matches) != 0 {
		t.Error("expected no matches from empty registry")
	}
}

// --- Replay on subscribe ---

// startReader drains envelopes from c into the returned channel until c
// closes. Must be called BEFORE anything writes to the opposite end —
// net.Pipe is synchronous, so Register→Replay→SendFireAndForget will
// block forever if no reader is waiting.
func startReader(t *testing.T, c *transport.Conn) <-chan *gcpc.EnvelopeV1 {
	t.Helper()
	ch := make(chan *gcpc.EnvelopeV1, 32)
	go func() {
		defer close(ch)
		for {
			env, err := c.Recv()
			if err != nil {
				return
			}
			ch <- env
		}
	}()
	return ch
}

// collect pulls up to want envelopes off ch within timeout, plus a short
// grace window afterwards to catch trailing deliveries. Used to assert on
// "exactly N envelopes arrived" without flaking.
func collect(t *testing.T, ch <-chan *gcpc.EnvelopeV1, want int, timeout time.Duration) []*gcpc.EnvelopeV1 {
	t.Helper()
	var out []*gcpc.EnvelopeV1
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case env, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, env)
		case <-deadline:
			return out
		}
	}
	grace := time.After(50 * time.Millisecond)
drain:
	for {
		select {
		case env, ok := <-ch:
			if !ok {
				break drain
			}
			out = append(out, env)
		case <-grace:
			break drain
		}
	}
	return out
}

func TestExecutor_ReplayDeliversActiveOpsInStartOrder(t *testing.T) {
	registry := NewRegistry()
	tracker := serverOps.NewTracker()

	// Three active ops that started before the plugin subscribes. Stagger
	// their StartTime via small sleeps so sort-by-start-order is observable.
	op1 := tracker.Start(ops.TypeCommand, "")
	time.Sleep(1 * time.Millisecond)
	op2 := tracker.Start(ops.TypeCommand, "")
	time.Sleep(1 * time.Millisecond)
	op3 := tracker.Start(ops.TypeCommand, "")

	exec := NewExecutor(registry, 100*time.Millisecond)
	exec.SetTracker(tracker)

	s, c := testPipe()
	defer c.Close()
	defer s.Close()

	pc := router.NewPluginConn("late", s)
	defer pc.Close()
	registry.SetOnRegister(exec.Replay)

	ch := startReader(t, c)

	// Act — Register triggers SetOnRegister → Replay.
	registry.Register("late", 10, pc, []string{"command"})

	envs := collect(t, ch, 3, 1*time.Second)
	if len(envs) != 3 {
		t.Fatalf("expected 3 replayed envelopes, got %d", len(envs))
	}

	wantIDs := []string{op1.ID, op2.ID, op3.ID}
	for i, env := range envs {
		hr := env.GetOperationHookRequest()
		if hr == nil {
			t.Fatalf("envelope[%d] is not an OperationHookRequest", i)
		}
		if !hr.Replayed {
			t.Errorf("envelope[%d] Replayed=false, want true", i)
		}
		if hr.Phase != "start" {
			t.Errorf("envelope[%d] phase=%q, want start", i, hr.Phase)
		}
		if hr.OperationId != wantIDs[i] {
			t.Errorf("envelope[%d] op_id=%q, want %q", i, hr.OperationId, wantIDs[i])
		}
		// Absolute wall-clock: must be within the test window, not zero.
		if hr.ReplayStartUnixNs <= 0 {
			t.Errorf("envelope[%d] ReplayStartUnixNs=%d, want >0", i, hr.ReplayStartUnixNs)
		}
		if hr.ReplayStartUnixNs < time.Now().Add(-1*time.Minute).UnixNano() {
			t.Errorf("envelope[%d] ReplayStartUnixNs=%d unreasonably old", i, hr.ReplayStartUnixNs)
		}
	}
}

func TestExecutor_ReplaySkipsOpsStartedAfterRegister(t *testing.T) {
	registry := NewRegistry()
	tracker := serverOps.NewTracker()
	exec := NewExecutor(registry, 100*time.Millisecond)
	exec.SetTracker(tracker)

	// Capture the regTime via a wrapper that also starts a fresh op after
	// registration lands. This op should NOT be in the replay set.
	var postRegOp *ops.Operation
	registry.SetOnRegister(func(pluginName string, regTime time.Time) {
		// Start a new op strictly after regTime — mirrors a live command
		// arriving the moment after a plugin finishes subscribing.
		time.Sleep(5 * time.Millisecond)
		postRegOp = tracker.Start(ops.TypeCommand, "")
		exec.Replay(pluginName, regTime)
	})

	op1 := tracker.Start(ops.TypeCommand, "")

	s, c := testPipe()
	defer c.Close()
	defer s.Close()
	pc := router.NewPluginConn("late", s)
	defer pc.Close()

	ch := startReader(t, c)
	registry.Register("late", 10, pc, []string{"command"})

	envs := collect(t, ch, 2, 500*time.Millisecond)
	if len(envs) != 1 {
		t.Fatalf("expected 1 replayed env (op1), got %d", len(envs))
	}
	hr := envs[0].GetOperationHookRequest()
	if hr.OperationId != op1.ID {
		t.Errorf("replayed op_id=%q, want %q (op1)", hr.OperationId, op1.ID)
	}
	if postRegOp != nil && hr.OperationId == postRegOp.ID {
		t.Error("post-register op should NOT be in replay set")
	}
}

func TestExecutor_ReplayFiltersByPluginPattern(t *testing.T) {
	registry := NewRegistry()
	tracker := serverOps.NewTracker()
	exec := NewExecutor(registry, 100*time.Millisecond)
	exec.SetTracker(tracker)

	_ = tracker.Start(ops.TypeCommand, "")  // should be replayed
	_ = tracker.Start(ops.TypeCleanup, "")  // should NOT match cmdonly
	_ = tracker.Start(ops.TypeSnapshot, "") // should NOT match cmdonly

	s, c := testPipe()
	defer c.Close()
	defer s.Close()
	pc := router.NewPluginConn("cmdonly", s)
	defer pc.Close()
	registry.SetOnRegister(exec.Replay)
	ch := startReader(t, c)

	registry.Register("cmdonly", 10, pc, []string{"command"})

	envs := collect(t, ch, 3, 500*time.Millisecond)
	if len(envs) != 1 {
		t.Fatalf("expected 1 replayed env (command only), got %d", len(envs))
	}
	hr := envs[0].GetOperationHookRequest()
	if hr.OperationType != "command" {
		t.Errorf("expected OperationType=command, got %q", hr.OperationType)
	}
}

func TestExecutor_ReplayNoOpWhenTrackerUnset(t *testing.T) {
	registry := NewRegistry()
	exec := NewExecutor(registry, 100*time.Millisecond)
	// Deliberately no SetTracker.

	s, c := testPipe()
	defer c.Close()
	defer s.Close()
	pc := router.NewPluginConn("p", s)
	defer pc.Close()

	registry.SetOnRegister(exec.Replay)
	ch := startReader(t, c)
	registry.Register("p", 10, pc, []string{"*"})

	// Nothing should arrive; poll briefly.
	envs := collect(t, ch, 1, 200*time.Millisecond)
	if len(envs) != 0 {
		t.Errorf("expected no replay without tracker, got %d envelopes", len(envs))
	}
}

func TestExecutor_ReplaySuppressedWithinRestartWindow(t *testing.T) {
	registry := NewRegistry()
	tracker := serverOps.NewTracker()
	exec := NewExecutor(registry, 100*time.Millisecond)
	exec.SetTracker(tracker)
	exec.SetMinRestartInterval(1 * time.Second)

	tracker.Start(ops.TypeCommand, "")

	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()
	pc1 := router.NewPluginConn("flappy", s1)
	defer pc1.Close()

	registry.SetOnRegister(exec.Replay)
	ch1 := startReader(t, c1)

	// First register fires a replay.
	registry.Register("flappy", 10, pc1, []string{"command"})
	envs := collect(t, ch1, 1, 500*time.Millisecond)
	if len(envs) != 1 {
		t.Fatalf("first register: expected 1 replayed env, got %d", len(envs))
	}

	// Second register within the suppression window: no replay. Use a
	// fresh pipe + PluginConn — a real re-register would come after the
	// previous conn died; reusing the first conn would race with its
	// reader goroutine on close.
	s2, c2 := testPipe()
	defer c2.Close()
	defer s2.Close()
	pc2 := router.NewPluginConn("flappy", s2)
	defer pc2.Close()
	// Unregister simulates the crash, then re-register to mimic restart.
	registry.Unregister("flappy")
	ch2 := startReader(t, c2)
	registry.Register("flappy", 10, pc2, []string{"command"})

	envs2 := collect(t, ch2, 1, 300*time.Millisecond)
	if len(envs2) != 0 {
		t.Errorf("re-register within window should skip replay, got %d envelopes", len(envs2))
	}
}

func TestExecutor_ReplayResumesAfterRestartWindow(t *testing.T) {
	registry := NewRegistry()
	tracker := serverOps.NewTracker()
	exec := NewExecutor(registry, 100*time.Millisecond)
	exec.SetTracker(tracker)
	exec.SetMinRestartInterval(50 * time.Millisecond)

	tracker.Start(ops.TypeCommand, "")

	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()
	pc1 := router.NewPluginConn("eventual", s1)
	defer pc1.Close()

	registry.SetOnRegister(exec.Replay)
	ch1 := startReader(t, c1)
	registry.Register("eventual", 10, pc1, []string{"command"})
	if len(collect(t, ch1, 1, 500*time.Millisecond)) != 1 {
		t.Fatal("first register expected a replay")
	}

	// Wait out the window then re-register on a new conn. Replay should fire again.
	time.Sleep(80 * time.Millisecond)

	s2, c2 := testPipe()
	defer c2.Close()
	defer s2.Close()
	pc2 := router.NewPluginConn("eventual", s2)
	defer pc2.Close()
	registry.Unregister("eventual")
	ch2 := startReader(t, c2)
	registry.Register("eventual", 10, pc2, []string{"command"})

	if got := len(collect(t, ch2, 1, 500*time.Millisecond)); got != 1 {
		t.Errorf("replay should resume after window, got %d envelopes", got)
	}
}

func TestExecutor_ReplayWildcardMatchesEveryType(t *testing.T) {
	registry := NewRegistry()
	tracker := serverOps.NewTracker()
	exec := NewExecutor(registry, 100*time.Millisecond)
	exec.SetTracker(tracker)

	tracker.Start(ops.TypeCommand, "")
	tracker.Start(ops.TypeCleanup, "")
	tracker.Start(ops.TypeSnapshot, "")

	s, c := testPipe()
	defer c.Close()
	defer s.Close()
	pc := router.NewPluginConn("wild", s)
	defer pc.Close()
	registry.SetOnRegister(exec.Replay)
	ch := startReader(t, c)

	registry.Register("wild", 10, pc, []string{"*"})

	envs := collect(t, ch, 3, 500*time.Millisecond)
	if len(envs) != 3 {
		t.Errorf("wildcard should replay all 3 types, got %d", len(envs))
	}
}
