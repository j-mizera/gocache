package events

import (
	"strconv"
	"testing"

	apiEvents "gocache/api/events"
)

func BenchmarkBusEmit(b *testing.B) {
	evt := apiEvents.NewCommandCompleted("PING", nil, 100, "PONG", "", nil)

	b.Run("no_subscribers", func(b *testing.B) {
		bus := NewBusWithCapacity(0)
		b.ResetTimer()
		for b.Loop() {
			bus.Emit(evt)
		}
	})
	b.Run("one_subscriber", func(b *testing.B) {
		bus := NewBusWithCapacity(0)
		bus.Subscribe("sub", []apiEvents.Type{apiEvents.CommandCompleted}, func(apiEvents.Event) {})
		b.ResetTimer()
		for b.Loop() {
			bus.Emit(evt)
		}
	})
	b.Run("many_subscribers", func(b *testing.B) {
		bus := NewBusWithCapacity(0)
		for i := 0; i < 16; i++ {
			bus.Subscribe("sub-"+strconv.Itoa(i), []apiEvents.Type{apiEvents.CommandCompleted}, func(apiEvents.Event) {})
		}
		b.ResetTimer()
		for b.Loop() {
			bus.Emit(evt)
		}
	})
	b.Run("type_interest_check", func(b *testing.B) {
		bus := NewBusWithCapacity(0)
		bus.Subscribe("sub", []apiEvents.Type{apiEvents.CommandCompleted}, func(apiEvents.Event) {})
		b.ResetTimer()
		for b.Loop() {
			_ = bus.HasSubscribersFor(apiEvents.CommandCompleted)
		}
	})
}
