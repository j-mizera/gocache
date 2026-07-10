package events

import (
	"fmt"
	"sync"
	"testing"

	apiEvents "gocache/api/events"
)

// BenchmarkTelemetryDeliveryLatency_Local measures FR-003 subscriber-round-trip latency,
// not fire-and-forget emit cost. The local baseline establishes the in-process floor;
// tmpfs-backed transports add IPC overhead above this path.
func BenchmarkTelemetryDeliveryLatency_Local(b *testing.B) {
	bus := NewBusWithCapacity(0)
	received := make(chan struct{}, 1)
	bus.Subscribe("bench-local", []apiEvents.Type{apiEvents.CommandCompleted}, func(apiEvents.Event) {
		received <- struct{}{}
	})
	evt := apiEvents.NewCommandCompleted("PING", nil, 100, "PONG", "", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bus.Emit(evt)
		<-received
	}
}

// BenchmarkTelemetryDeliveryFanout measures FR-003 emit-to-all-subscribers receipt,
// not fire-and-forget emit cost. Each iteration waits for every in-process subscriber;
// tmpfs-backed transports add IPC overhead above this local fan-out floor.
func BenchmarkTelemetryDeliveryFanout(b *testing.B) {
	subscriberCounts := []int{1, 10, 100}
	evt := apiEvents.NewCommandCompleted("PING", nil, 100, "PONG", "", nil)

	for _, subscriberCount := range subscriberCounts {
		b.Run(fmt.Sprintf("Subscribers_%d", subscriberCount), func(b *testing.B) {
			bus := NewBusWithCapacity(0)
			var waitGroup sync.WaitGroup
			for i := 0; i < subscriberCount; i++ {
				bus.Subscribe(fmt.Sprintf("sub-%d", i), []apiEvents.Type{apiEvents.CommandCompleted}, func(apiEvents.Event) {
					waitGroup.Done()
				})
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				waitGroup.Add(subscriberCount)
				bus.Emit(evt)
				waitGroup.Wait()
			}
		})
	}
}
