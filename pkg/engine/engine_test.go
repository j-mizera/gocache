package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gocache/pkg/cache"
)

func newTestEngine(t *testing.T) (*Engine, *cache.Cache) {
	t.Helper()
	c := cache.New()
	e := New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })
	return e, c
}

func TestDispatchWithResult(t *testing.T) {
	e, _ := newTestEngine(t)

	res, err := e.DispatchWithResult(context.Background(), func() any {
		return 42
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != 42 {
		t.Errorf("expected 42, got %v", res)
	}
}

func TestDispatch(t *testing.T) {
	e, _ := newTestEngine(t)

	var called atomic.Bool
	if err := e.Dispatch(context.Background(), func() {
		called.Store(true)
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Dispatch is synchronous — by the time it returns, fn has run.
	if !called.Load() {
		t.Error("expected Dispatch to execute the function")
	}
}

func TestDispatchWithResult_Serialization(t *testing.T) {
	e, _ := newTestEngine(t)

	var counter int64
	done := make(chan struct{})
	const n = 100

	go func() {
		for range n {
			_, _ = e.DispatchWithResult(context.Background(), func() any {
				counter++
				return nil
			})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for dispatches")
	}

	if counter != n {
		t.Errorf("expected counter %d, got %d", n, counter)
	}
}

func TestStop_ReturnsEngineStopped(t *testing.T) {
	c := cache.New()
	e := New(c)
	go e.Run()

	e.Stop()

	// After stop, DispatchWithResult should return ErrEngineStopped.
	res, err := e.DispatchWithResult(context.Background(), func() any {
		return "should not run"
	})
	if res != nil {
		t.Errorf("expected nil result after stop, got %v", res)
	}
	if !errors.Is(err, ErrEngineStopped) {
		t.Errorf("expected ErrEngineStopped, got %v", err)
	}
}

func TestDispatch_CtxCancelled(t *testing.T) {
	e, _ := newTestEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := e.Dispatch(ctx, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestStop_Idempotent(t *testing.T) {
	c := cache.New()
	e := New(c)
	go e.Run()

	e.Stop()
	e.Stop() // should not panic
}

// TestSendAndWait_PoolSafety_CancelDuringWait pins down the resChan-pool
// safety rule: when ctx.Done() fires AFTER the engine has taken ownership
// of the result channel but BEFORE sendAndWait observed the result, the
// channel must NOT be returned to the pool — the engine may still write to
// it. The next caller must always Get a clean channel.
//
// We verify by stalling the engine inside its execute callback, cancelling
// the context, then issuing a fresh dispatch and asserting it gets a
// distinct (or at minimum drained) channel.
func TestSendAndWait_PoolSafety_CancelDuringWait(t *testing.T) {
	e, _ := newTestEngine(t)

	// Hold the engine inside its execute callback long enough for ctx to cancel.
	ctx1, cancel1 := context.WithCancel(context.Background())
	enter := make(chan struct{})
	release := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		_, _ = e.DispatchWithResult(ctx1, func() any {
			close(enter)
			<-release
			return "stalled-result"
		})
		close(doneCh)
	}()

	<-enter            // engine is now executing fn, holding resChan
	cancel1()          // cancellation fires while engine still owns the channel
	<-doneCh           // sendAndWait returned ctx.Err(); resChan was orphaned
	close(release)     // let the engine finish writing to the orphaned channel

	// Drain. The engine wrote "stalled-result" into the orphaned channel
	// after sendAndWait returned. Any next dispatch must NOT see that
	// stale value.
	res, err := e.DispatchWithResult(context.Background(), func() any { return "fresh" })
	if err != nil {
		t.Fatalf("post-cancel dispatch error: %v", err)
	}
	if res != "fresh" {
		t.Fatalf("post-cancel dispatch leaked stale value: got %v", res)
	}
}

// TestSendAndWait_PoolSafety_StopDuringWait mirrors the cancel-path test
// for the engine-stop branch. Same rule, same hazard.
func TestSendAndWait_PoolSafety_StopDuringWait(t *testing.T) {
	c := cache.New()
	e := New(c)
	go e.Run()

	enter := make(chan struct{})
	release := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		_, _ = e.DispatchWithResult(context.Background(), func() any {
			close(enter)
			<-release
			return "stalled-result"
		})
		close(doneCh)
	}()

	<-enter            // engine is executing fn
	e.Stop()           // stop fires while engine holds the channel
	<-doneCh           // sendAndWait returned ErrEngineStopped
	close(release)     // engine finishes writing to the orphaned channel

	// After stop the engine is gone, so a follow-up dispatch returns
	// ErrEngineStopped instead of running. The important guarantee is
	// "no panic, no goroutine leak"; both are observable here because
	// the test would deadlock otherwise.
	_, err := e.DispatchWithResult(context.Background(), func() any { return "fresh" })
	if !errors.Is(err, ErrEngineStopped) {
		t.Fatalf("expected ErrEngineStopped after stop, got %v", err)
	}
}

// TestSendAndWait_PoolReuse_Sequential exercises the success path's Put
// and asserts that sequential dispatches reuse channels from the pool
// without leaking results. Concurrent correctness is covered by -race
// across the existing serialization test.
func TestSendAndWait_PoolReuse_Sequential(t *testing.T) {
	e, _ := newTestEngine(t)

	const n = 100
	for i := 0; i < n; i++ {
		res, err := e.DispatchWithResult(context.Background(), func() any { return i })
		if err != nil {
			t.Fatalf("iter %d: dispatch error %v", i, err)
		}
		if res != i {
			t.Fatalf("iter %d: stale value: got %v, want %d", i, res, i)
		}
	}
}
