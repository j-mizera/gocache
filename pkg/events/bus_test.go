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

// --- Replay ring ---

func TestBus_ReplayDeliversRetainedEventsInFIFOOrder(t *testing.T) {
	bus := NewBusWithCapacity(10)

	// Emit before anyone subscribes — the ring must hold these.
	bus.Emit(apiEvents.NewConnectionOpen("a"))
	bus.Emit(apiEvents.NewConnectionOpen("b"))
	bus.Emit(apiEvents.NewConnectionOpen("c"))

	var got []string
	bus.Subscribe("late", []apiEvents.Type{apiEvents.ConnectionOpen}, func(evt apiEvents.Event) {
		got = append(got, evt.Proto.GetConnectionOpen().RemoteAddr)
	})

	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("replay: got %d events, want %d (%v)", len(got), len(want), got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("replay[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestBus_ReplayFiltersByType(t *testing.T) {
	bus := NewBusWithCapacity(10)

	bus.Emit(apiEvents.NewServerStart("addr", "v1"))
	bus.Emit(apiEvents.NewConnectionOpen("a"))
	bus.Emit(apiEvents.NewConnectionClose("a", 1))

	var count int
	bus.Subscribe("only-conn-open", []apiEvents.Type{apiEvents.ConnectionOpen}, func(evt apiEvents.Event) {
		count++
	})

	if count != 1 {
		t.Errorf("expected only ConnectionOpen replayed, got %d total events", count)
	}
}

func TestBus_ReplayNoDuplicatesWithConcurrentEmit(t *testing.T) {
	bus := NewBusWithCapacity(200)
	// Pre-seed 10 events.
	for i := range 10 {
		_ = i
		bus.Emit(apiEvents.NewConnectionOpen("x"))
	}

	var received atomic.Int32
	// Subscribe while concurrent emits are racing. The lock in Subscribe
	// prevents a live Emit from interleaving — any post-Subscribe emit
	// lands live, any pre-Subscribe emit lands in the replay snapshot.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 40 {
			_ = i
			bus.Emit(apiEvents.NewConnectionOpen("y"))
		}
	}()

	bus.Subscribe("counter", []apiEvents.Type{apiEvents.ConnectionOpen}, func(evt apiEvents.Event) {
		received.Add(1)
	})
	wg.Wait()

	// Exactly 50 emits total (10 pre + 40 concurrent). Replay covers every
	// emit that committed before Subscribe grabbed the lock; live delivery
	// covers everything after. No event should be seen twice.
	if got := received.Load(); got != 50 {
		t.Errorf("expected 50 total events, got %d", got)
	}
}

func TestBus_OverflowDropsOldestAndEmitsReplayGap(t *testing.T) {
	bus := NewBusWithCapacity(3)

	bus.Emit(apiEvents.NewConnectionOpen("a")) // dropped
	bus.Emit(apiEvents.NewConnectionOpen("b")) // dropped
	bus.Emit(apiEvents.NewConnectionOpen("c"))
	bus.Emit(apiEvents.NewConnectionOpen("d"))
	bus.Emit(apiEvents.NewConnectionOpen("e"))

	var addrs []string
	var gapCount uint64
	bus.Subscribe("late",
		[]apiEvents.Type{apiEvents.ConnectionOpen, apiEvents.ReplayGap},
		func(evt apiEvents.Event) {
			switch apiEvents.Type(evt.Proto.Type) {
			case apiEvents.ReplayGap:
				// Dropped count is smuggled in LogEntry fields in Phase B.
				fields := evt.Proto.GetLogEntry().GetFields()
				if v, ok := fields["_dropped_count"]; ok {
					var n uint64
					for _, ch := range v {
						n = n*10 + uint64(ch-'0')
					}
					gapCount = n
				}
			case apiEvents.ConnectionOpen:
				addrs = append(addrs, evt.Proto.GetConnectionOpen().RemoteAddr)
			}
		})

	if gapCount != 2 {
		t.Errorf("expected dropped count 2, got %d", gapCount)
	}
	want := []string{"c", "d", "e"}
	if len(addrs) != len(want) {
		t.Fatalf("addrs = %v, want %v", addrs, want)
	}
	for i, v := range want {
		if addrs[i] != v {
			t.Errorf("addrs[%d] = %q, want %q", i, addrs[i], v)
		}
	}
}

func TestBus_ZeroCapacityDisablesReplay(t *testing.T) {
	bus := NewBusWithCapacity(0)
	bus.Emit(apiEvents.NewConnectionOpen("a"))

	var received atomic.Int32
	bus.Subscribe("late", []apiEvents.Type{apiEvents.ConnectionOpen}, func(evt apiEvents.Event) {
		received.Add(1)
	})
	if received.Load() != 0 {
		t.Errorf("expected zero replay events, got %d", received.Load())
	}

	// Live delivery still works.
	bus.Emit(apiEvents.NewConnectionOpen("b"))
	if received.Load() != 1 {
		t.Errorf("expected 1 live event, got %d", received.Load())
	}
}

func TestBus_ReplayGapOnlyWhenSubscribed(t *testing.T) {
	bus := NewBusWithCapacity(2)
	bus.Emit(apiEvents.NewConnectionOpen("a")) // dropped
	bus.Emit(apiEvents.NewConnectionOpen("b"))
	bus.Emit(apiEvents.NewConnectionOpen("c"))

	// Subscriber does NOT list ReplayGap — it should see conn events only.
	var saw []string
	bus.Subscribe("no-gap", []apiEvents.Type{apiEvents.ConnectionOpen}, func(evt apiEvents.Event) {
		saw = append(saw, evt.Proto.Type)
	})
	for _, s := range saw {
		if s == string(apiEvents.ReplayGap) {
			t.Fatalf("unsubscribed type delivered: %v", saw)
		}
	}
	if len(saw) != 2 {
		t.Errorf("expected 2 retained events, got %d", len(saw))
	}
}

func TestBus_UpdateSubscriptionDoesNotReplay(t *testing.T) {
	bus := NewBusWithCapacity(10)
	var received atomic.Int32
	bus.Subscribe("same", []apiEvents.Type{apiEvents.ConnectionOpen}, func(evt apiEvents.Event) {
		received.Add(1)
	})
	bus.Emit(apiEvents.NewConnectionOpen("a"))
	bus.Emit(apiEvents.NewConnectionOpen("b"))
	// Re-register under the same name — assumed already caught up.
	bus.Subscribe("same", []apiEvents.Type{apiEvents.ConnectionOpen}, func(evt apiEvents.Event) {
		received.Add(1)
	})
	if received.Load() != 2 {
		t.Errorf("expected 2 events (live only, no replay on update), got %d", received.Load())
	}
}
