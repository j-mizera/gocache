package blocking

import (
	"testing"
	"time"
)

func TestRegisterAndTryWake(t *testing.T) {
	r := NewRegistry()

	ch, cancel := r.Register([]string{"list1"})
	defer cancel()

	wCh, ok := r.TryWake("list1")
	if !ok {
		t.Fatal("expected TryWake to find a waiter")
	}

	wCh <- WakeResult{Key: "list1", Value: "hello"}

	select {
	case res := <-ch:
		if res.Key != "list1" || res.Value != "hello" {
			t.Errorf("unexpected result: %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for wake result")
	}
}

func TestTryWake_NoWaiters(t *testing.T) {
	r := NewRegistry()

	_, ok := r.TryWake("nokey")
	if ok {
		t.Error("expected TryWake to return false for empty key")
	}
}

func TestTryWake_FIFO(t *testing.T) {
	r := NewRegistry()

	ch1, cancel1 := r.Register([]string{"key"})
	defer cancel1()
	ch2, cancel2 := r.Register([]string{"key"})
	defer cancel2()

	// First TryWake should return ch1 (FIFO).
	wCh, ok := r.TryWake("key")
	if !ok {
		t.Fatal("expected waiter")
	}
	wCh <- WakeResult{Key: "key", Value: "first"}

	select {
	case res := <-ch1:
		if res.Value != "first" {
			t.Errorf("expected first waiter, got %+v", res)
		}
	case <-ch2:
		t.Error("second waiter should not have been woken first")
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestCancel_RemovesFromAllKeys(t *testing.T) {
	r := NewRegistry()

	_, cancel := r.Register([]string{"a", "b", "c"})
	cancel()

	for _, key := range []string{"a", "b", "c"} {
		_, ok := r.TryWake(key)
		if ok {
			t.Errorf("expected no waiter for %s after cancel", key)
		}
	}
}

func TestMultiKeyRegistration(t *testing.T) {
	r := NewRegistry()

	ch, cancel := r.Register([]string{"x", "y"})
	defer cancel()

	// Wake via second key.
	wCh, ok := r.TryWake("y")
	if !ok {
		t.Fatal("expected waiter on y")
	}
	wCh <- WakeResult{Key: "y", Value: "val"}

	res := <-ch
	if res.Key != "y" {
		t.Errorf("expected key y, got %s", res.Key)
	}

	// Waiter should be removed from key x too.
	_, ok = r.TryWake("x")
	if ok {
		t.Error("expected waiter removed from x after wake on y")
	}
}

func TestShutdown(t *testing.T) {
	r := NewRegistry()
	r.Shutdown()

	select {
	case <-r.Done():
	default:
		t.Error("expected Done channel to be closed after Shutdown")
	}
}
