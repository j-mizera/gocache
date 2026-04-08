package engine

import (
	"gocache/pkg/cache"
	"sync/atomic"
	"testing"
	"time"
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

	res := e.DispatchWithResult(func() interface{} {
		return 42
	})
	if res != 42 {
		t.Errorf("expected 42, got %v", res)
	}
}

func TestDispatch(t *testing.T) {
	e, _ := newTestEngine(t)

	var called atomic.Bool
	e.Dispatch(func() {
		called.Store(true)
	})
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
			e.DispatchWithResult(func() interface{} {
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

func TestStop_ReturnsNil(t *testing.T) {
	c := cache.New()
	e := New(c)
	go e.Run()

	e.Stop()

	// After stop, DispatchWithResult should return nil.
	res := e.DispatchWithResult(func() interface{} {
		return "should not run"
	})
	if res != nil {
		t.Errorf("expected nil after stop, got %v", res)
	}
}

func TestStop_Idempotent(t *testing.T) {
	c := cache.New()
	e := New(c)
	go e.Run()

	e.Stop()
	e.Stop() // should not panic
}
