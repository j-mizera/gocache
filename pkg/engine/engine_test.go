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
	e.Stop()

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
	e.Stop()
	e.Stop() // should not panic
}

func TestDispatchToShard(t *testing.T) {
	e, _ := newTestEngine(t)

	res, err := e.DispatchToShard(context.Background(), 0, func() any {
		return "hello"
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "hello" {
		t.Errorf("expected hello, got %v", res)
	}
}

func TestDispatchToShard_Stopped(t *testing.T) {
	c := cache.New()
	e := New(c)
	e.Stop()

	_, err := e.DispatchToShard(context.Background(), 0, func() any { return nil })
	if !errors.Is(err, ErrEngineStopped) {
		t.Errorf("expected ErrEngineStopped, got %v", err)
	}
}

func TestDispatchToShard_CtxCancelled(t *testing.T) {
	e, _ := newTestEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.DispatchToShard(ctx, 0, func() any { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestDispatchToShard_Sequential(t *testing.T) {
	e, _ := newTestEngine(t)

	const n = 100
	for i := 0; i < n; i++ {
		res, err := e.DispatchToShard(context.Background(), 0, func() any { return i })
		if err != nil {
			t.Fatalf("iter %d: dispatch error %v", i, err)
		}
		if res != i {
			t.Fatalf("iter %d: got %v, want %d", i, res, i)
		}
	}
}

func TestDispatchToShardRO(t *testing.T) {
	e, _ := newTestEngine(t)

	res, err := e.DispatchToShardRO(context.Background(), 0, func() any {
		return "read"
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "read" {
		t.Errorf("expected read, got %v", res)
	}
}

func TestDispatchToShardRO_Stopped(t *testing.T) {
	c := cache.New()
	e := New(c)
	e.Stop()

	_, err := e.DispatchToShardRO(context.Background(), 0, func() any { return nil })
	if !errors.Is(err, ErrEngineStopped) {
		t.Errorf("expected ErrEngineStopped, got %v", err)
	}
}

func TestDispatchToShardRO_CtxCancelled(t *testing.T) {
	e, _ := newTestEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.DispatchToShardRO(ctx, 0, func() any { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestDispatchToShardRO_ConcurrentReads(t *testing.T) {
	e, _ := newTestEngine(t)

	const readers = 50
	start := make(chan struct{})
	done := make(chan struct{}, readers)

	for range readers {
		go func() {
			<-start
			_, err := e.DispatchToShardRO(context.Background(), 0, func() any {
				return "ok"
			})
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			done <- struct{}{}
		}()
	}

	close(start)
	for range readers {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for concurrent reads")
		}
	}
}

func TestAcquireShard(t *testing.T) {
	e, _ := newTestEngine(t)

	release := e.AcquireShard(0)
	release()
}

func TestAcquireShardRO(t *testing.T) {
	e, _ := newTestEngine(t)

	release := e.AcquireShardRO(0)
	release()
}

func TestDispatchToShards(t *testing.T) {
	e, _ := newTestEngine(t)
	shards := []int{0, 1}
	if e.ShardCount() < 2 {
		shards = []int{0}
	}

	res, err := e.DispatchToShards(context.Background(), shards, func() any {
		return "multi"
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "multi" {
		t.Errorf("expected multi, got %v", res)
	}
}
