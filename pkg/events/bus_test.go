package events

import (
	"sync"
	"sync/atomic"
	"testing"

	apiEvents "gocache/api/events"
)

func TestBus_EmitNoSubscribers(t *testing.T) {
	bus := NewBus()
	// Should not panic.
	bus.Emit(apiEvents.NewServerStart("localhost:6379", "1.0"))
}

func TestBus_SubscribeAndEmit(t *testing.T) {
	bus := NewBus()
	var received atomic.Int32

	bus.Subscribe("test", []apiEvents.Type{apiEvents.ServerStart}, func(evt apiEvents.Event) {
		received.Add(1)
	})

	bus.Emit(apiEvents.NewServerStart("localhost:6379", "1.0"))
	if received.Load() != 1 {
		t.Errorf("expected 1 event, got %d", received.Load())
	}
}

func TestBus_TypeFiltering(t *testing.T) {
	bus := NewBus()
	var received atomic.Int32

	bus.Subscribe("test", []apiEvents.Type{apiEvents.ConnectionOpen}, func(evt apiEvents.Event) {
		received.Add(1)
	})

	// Emit a different type — subscriber should not receive it.
	bus.Emit(apiEvents.NewServerStart("localhost:6379", "1.0"))
	if received.Load() != 0 {
		t.Errorf("expected 0 events, got %d", received.Load())
	}

	// Emit the subscribed type.
	bus.Emit(apiEvents.NewConnectionOpen("127.0.0.1:1234"))
	if received.Load() != 1 {
		t.Errorf("expected 1 event, got %d", received.Load())
	}
}

func TestBus_MultipleSubscribers(t *testing.T) {
	bus := NewBus()
	var count1, count2 atomic.Int32

	bus.Subscribe("sub1", []apiEvents.Type{apiEvents.CommandPost}, func(evt apiEvents.Event) {
		count1.Add(1)
	})
	bus.Subscribe("sub2", []apiEvents.Type{apiEvents.CommandPost}, func(evt apiEvents.Event) {
		count2.Add(1)
	})

	bus.Emit(apiEvents.NewCommandPost("SET", []string{"k", "v"}, 1000, "OK", "", nil))

	if count1.Load() != 1 || count2.Load() != 1 {
		t.Errorf("expected both subscribers to receive: sub1=%d, sub2=%d", count1.Load(), count2.Load())
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	bus := NewBus()
	var received atomic.Int32

	bus.Subscribe("test", []apiEvents.Type{apiEvents.ServerShutdown}, func(evt apiEvents.Event) {
		received.Add(1)
	})

	bus.Unsubscribe("test")
	bus.Emit(apiEvents.NewServerShutdown("signal"))

	if received.Load() != 0 {
		t.Errorf("expected 0 after unsubscribe, got %d", received.Load())
	}
}

func TestBus_HasSubscribers(t *testing.T) {
	bus := NewBus()
	if bus.HasSubscribers() {
		t.Error("expected no subscribers")
	}

	bus.Subscribe("test", []apiEvents.Type{apiEvents.LogEntry}, func(evt apiEvents.Event) {})
	if !bus.HasSubscribers() {
		t.Error("expected subscribers")
	}

	bus.Unsubscribe("test")
	if bus.HasSubscribers() {
		t.Error("expected no subscribers after unsubscribe")
	}
}

func TestBus_PanicRecovery(t *testing.T) {
	bus := NewBus()

	bus.Subscribe("panicker", []apiEvents.Type{apiEvents.ServerStart}, func(evt apiEvents.Event) {
		panic("boom")
	})

	// Should not panic the caller.
	bus.Emit(apiEvents.NewServerStart("localhost:6379", "1.0"))
}

func TestBus_ConcurrentEmit(t *testing.T) {
	bus := NewBus()
	var received atomic.Int32

	bus.Subscribe("counter", []apiEvents.Type{apiEvents.CommandPost}, func(evt apiEvents.Event) {
		received.Add(1)
	})

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bus.Emit(apiEvents.NewCommandPost("PING", nil, 100, "PONG", "", nil))
		}()
	}
	wg.Wait()

	if received.Load() != 50 {
		t.Errorf("expected 50, got %d", received.Load())
	}
}

func TestBus_MultipleEventTypes(t *testing.T) {
	bus := NewBus()
	var received atomic.Int32

	bus.Subscribe("multi", []apiEvents.Type{
		apiEvents.ConnectionOpen,
		apiEvents.ConnectionClose,
		apiEvents.AuthFailed,
	}, func(evt apiEvents.Event) {
		received.Add(1)
	})

	bus.Emit(apiEvents.NewConnectionOpen("127.0.0.1:1234"))
	bus.Emit(apiEvents.NewConnectionClose("127.0.0.1:1234", 5000))
	bus.Emit(apiEvents.NewAuthFailed("127.0.0.1:1234", "SET"))
	bus.Emit(apiEvents.NewServerStart("localhost:6379", "1.0")) // not subscribed

	if received.Load() != 3 {
		t.Errorf("expected 3, got %d", received.Load())
	}
}
