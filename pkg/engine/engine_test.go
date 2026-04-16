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

	res, err := e.DispatchWithResult(context.Background(), func() interface{} {
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
			_, _ = e.DispatchWithResult(context.Background(), func() interface{} {
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
	res, err := e.DispatchWithResult(context.Background(), func() interface{} {
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
