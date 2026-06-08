package events

import (
	"testing"

	apiEvents "gocache/api/events"
)

func TestBus_ReplayRetainsCommandPayloadsAfterProducerMutation(t *testing.T) {
	bus := NewBusWithCapacity(2)
	args := []string{"key", "value"}
	metadata := map[string]string{"shared.traceparent": "00-original"}

	bus.Emit(apiEvents.NewCommandStarted("SET", args, metadata))
	args[0] = "mutated-key"
	metadata["shared.traceparent"] = "00-mutated"
	metadata["shared.extra"] = "late"

	var replayed apiEvents.Event
	bus.Subscribe("late", []apiEvents.Type{apiEvents.CommandStarted}, func(evt apiEvents.Event) {
		replayed = evt
	})

	payload := replayed.Proto.GetCommandPre()
	if payload == nil {
		t.Fatal("replayed command.started payload missing")
	}
	if got := payload.Args[0]; got != "key" {
		t.Fatalf("replayed args[0] = %q, want immutable original", got)
	}
	if got := payload.Metadata["shared.traceparent"]; got != "00-original" {
		t.Fatalf("replayed metadata traceparent = %q, want immutable original", got)
	}
	if _, ok := payload.Metadata["shared.extra"]; ok {
		t.Fatalf("replayed metadata includes producer mutation: %+v", payload.Metadata)
	}
}

func TestBus_ReplayRetainsOperationContextAfterProducerMutation(t *testing.T) {
	bus := NewBusWithCapacity(2)
	operationContext := map[string]string{"tenant": "acme", "shared.traceparent": "00-original"}

	bus.Emit(apiEvents.NewOperationCompleted("op_1", "command", 42, "completed", "", operationContext))
	operationContext["tenant"] = "globex"
	operationContext["shared.extra"] = "late"

	var replayed apiEvents.Event
	bus.Subscribe("late", []apiEvents.Type{apiEvents.OperationCompleted}, func(evt apiEvents.Event) {
		replayed = evt
	})

	payload := replayed.Proto.GetOperationComplete()
	if payload == nil {
		t.Fatal("replayed operation.completed payload missing")
	}
	if got := payload.Context["tenant"]; got != "acme" {
		t.Fatalf("replayed context tenant = %q, want immutable original", got)
	}
	if _, ok := payload.Context["shared.extra"]; ok {
		t.Fatalf("replayed context includes producer mutation: %+v", payload.Context)
	}
}
