package persistence

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apipersistence "gocache/api/persistence"
)

// recordingSink captures every Apply batch in memory. Used by tests to
// assert ordering, batch shape, and lifecycle.
//
// All fields are guarded by mu so tests that mutate sink behaviour
// concurrently with the running flush goroutine (e.g. setting applyErr
// to nil after a transient failure) do not race.
type recordingSink struct {
	name   string
	policy apipersistence.FsyncPolicy

	mu         sync.Mutex
	batches    [][]apipersistence.Mutation
	closed     bool
	applyDelay time.Duration
	applyErr   error

	// applyCount counts Apply calls. Atomic so tests can assert without
	// holding mu (the count itself is independent of payload state).
	applyCount atomic.Int32
}

// setApplyErr is the race-safe way to mutate applyErr after Start.
func (s *recordingSink) setApplyErr(err error) {
	s.mu.Lock()
	s.applyErr = err
	s.mu.Unlock()
}

func newRecordingSink(name string) *recordingSink {
	return &recordingSink{name: name}
}

func (s *recordingSink) Name() string                       { return s.name }
func (s *recordingSink) FsyncPolicy() apipersistence.FsyncPolicy { return s.policy }
func (s *recordingSink) Apply(_ context.Context, batch []apipersistence.Mutation) error {
	s.applyCount.Add(1)
	s.mu.Lock()
	delay := s.applyDelay
	err := s.applyErr
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Copy the batch — the coordinator reuses its slice across Apply.
	cp := make([]apipersistence.Mutation, len(batch))
	copy(cp, batch)
	s.batches = append(s.batches, cp)
	return nil
}
func (s *recordingSink) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *recordingSink) totalMutations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.batches {
		n += len(b)
	}
	return n
}

func (s *recordingSink) snapshotBatches() [][]apipersistence.Mutation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]apipersistence.Mutation, len(s.batches))
	for i, b := range s.batches {
		cp := make([]apipersistence.Mutation, len(b))
		copy(cp, b)
		out[i] = cp
	}
	return out
}

func TestCoordinator_HasSinks_NoSinks(t *testing.T) {
	c := New(nil)
	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	if c.HasSinks() {
		t.Error("HasSinks reported true with no sinks registered")
	}
}

func TestCoordinator_HasSinks_AfterStart(t *testing.T) {
	sink := newRecordingSink("test")
	c := New(nil, sink)

	if c.HasSinks() {
		t.Error("HasSinks should be false before Start")
	}

	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	if !c.HasSinks() {
		t.Error("HasSinks should be true after Start with one sink")
	}
}

func TestCoordinator_Emit_Batches_TimeTrigger(t *testing.T) {
	sink := newRecordingSink("test")
	c := New(nil, sink)
	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	// Emit a few small mutations — well under the 64KB size trigger.
	// They should batch and flush on the 1ms timer.
	for i := 0; i < 5; i++ {
		c.AllocateAndEmit("SET", "key", [][]byte{[]byte("k"), []byte("v")})
	}

	// Wait for the timer-triggered flush. 50ms is generous.
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sink.totalMutations() == 5 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := sink.totalMutations(); got != 5 {
		t.Errorf("totalMutations = %d, want 5 after time-trigger flush", got)
	}
}

func TestCoordinator_Emit_Batches_SizeTrigger(t *testing.T) {
	sink := newRecordingSink("test")
	c := New(nil, sink)
	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	// Emit one mutation that crosses the 64KB threshold by itself —
	// guaranteed to fire the size trigger before the time trigger.
	bigArg := make([]byte, 70*1024)
	c.AllocateAndEmit("SET", "key", [][]byte{[]byte("k"), bigArg})

	// Size-triggered flush should be near-instant.
	deadline := time.Now().Add(20 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sink.applyCount.Load() == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("size-trigger flush did not fire; applyCount = %d", sink.applyCount.Load())
}

func TestCoordinator_Emit_LSN_Order_Sequential(t *testing.T) {
	sink := newRecordingSink("test")
	c := New(nil, sink)
	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	const total = 100
	for i := 0; i < total; i++ {
		c.AllocateAndEmit("SET", fmt.Sprintf("k%d", i), [][]byte{[]byte("v")})
	}

	// Drain.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sink.totalMutations() >= total {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// LSNs must be strictly increasing across the captured batches.
	var prev apipersistence.LSN
	for _, batch := range sink.snapshotBatches() {
		for _, m := range batch {
			if m.LSN <= prev {
				t.Fatalf("LSN went backwards: prev=%d cur=%d", prev, m.LSN)
			}
			prev = m.LSN
		}
	}
}

func TestCoordinator_Stop_Drains(t *testing.T) {
	sink := newRecordingSink("test")
	c := New(nil, sink)
	c.Start(context.Background())

	const total = 50
	for i := 0; i < total; i++ {
		c.AllocateAndEmit("SET", fmt.Sprintf("k%d", i), [][]byte{[]byte("v")})
	}

	c.Stop(context.Background())

	if got := sink.totalMutations(); got != total {
		t.Errorf("Stop did not drain: totalMutations = %d, want %d", got, total)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.closed {
		t.Error("Stop did not call Sink.Close")
	}
}

func TestCoordinator_MultiSink_FanOut(t *testing.T) {
	sinkA := newRecordingSink("A")
	sinkB := newRecordingSink("B")
	c := New(nil, sinkA, sinkB)
	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	const total = 10
	for i := 0; i < total; i++ {
		c.AllocateAndEmit("SET", fmt.Sprintf("k%d", i), [][]byte{[]byte("v")})
	}

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sinkA.totalMutations() == total && sinkB.totalMutations() == total {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("multi-sink fan-out incomplete: A=%d B=%d (want %d each)",
		sinkA.totalMutations(), sinkB.totalMutations(), total)
}

func TestCoordinator_FatalError_Quarantines(t *testing.T) {
	sink := newRecordingSink("fatal")
	sink.setApplyErr(fmt.Errorf("boom: %w", apipersistence.ErrSinkFatal))
	c := New(nil, sink)
	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	// First flush triggers the fatal error.
	c.AllocateAndEmit("SET", "k", [][]byte{[]byte("v")})

	// Wait for the flush + quarantine to settle.
	time.Sleep(20 * time.Millisecond)

	// Subsequent emits should not pile up behind the dead sink.
	for i := 0; i < 100; i++ {
		c.AllocateAndEmit("SET", "k", [][]byte{[]byte("v")})
	}
	// If quarantine didn't drain, the producer would block here on the
	// channel-send — we'd never reach this assertion. Reaching here is
	// the assertion: the producer never blocked.

	// applyCount should be exactly 1 (the failed Apply) — no retries
	// against a quarantined sink.
	time.Sleep(10 * time.Millisecond)
	if got := sink.applyCount.Load(); got != 1 {
		t.Errorf("applyCount = %d after quarantine; want 1 (no further Apply calls)", got)
	}
}

func TestCoordinator_NonFatalError_KeepsLooping(t *testing.T) {
	sink := newRecordingSink("transient")
	sink.setApplyErr(errors.New("transient failure"))
	c := New(nil, sink)
	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	c.AllocateAndEmit("SET", "k", [][]byte{[]byte("v")})
	time.Sleep(20 * time.Millisecond)

	// Clear the error so the next Apply succeeds.
	sink.setApplyErr(nil)

	c.AllocateAndEmit("SET", "k2", [][]byte{[]byte("v")})
	time.Sleep(20 * time.Millisecond)

	if got := sink.applyCount.Load(); got < 2 {
		t.Errorf("non-fatal error stopped the loop: applyCount = %d, want >=2", got)
	}
	if got := sink.totalMutations(); got < 1 {
		t.Errorf("totalMutations = %d, want >=1 after transient failure recovery", got)
	}
}

func TestCoordinator_Emit_Concurrent(t *testing.T) {
	sink := newRecordingSink("concurrent")
	c := New(nil, sink)
	c.Start(context.Background())
	t.Cleanup(func() { c.Stop(context.Background()) })

	const goroutines = 50
	const perG = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				c.AllocateAndEmit("SET", fmt.Sprintf("k-%d-%d", g, i), [][]byte{[]byte("v")})
			}
		}(g)
	}
	wg.Wait()

	c.Stop(context.Background())

	if got, want := sink.totalMutations(), goroutines*perG; got != want {
		t.Errorf("concurrent emit lost mutations: got %d, want %d", got, want)
	}
}
