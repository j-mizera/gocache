// Package events implements the server-wide event bus.
//
// Any server component can emit and subscribe to events. The plugin system
// bridges the bus to GCPC for remote plugin delivery, but the bus itself
// is not plugin-specific.
package events

import (
	"sync"

	apiEvents "gocache/api/events"
	"gocache/api/logger"
)

// Handler is a function that processes an event. Must be non-blocking.
type Handler func(apiEvents.Event)

// Subscription represents a component's interest in specific event types.
type Subscription struct {
	Name    string
	Types   map[apiEvents.Type]bool
	Handler Handler
}

// Bus dispatches events to subscribers. It implements api/events.Emitter.
type Bus struct {
	mu          sync.RWMutex
	subscribers map[string]*Subscription
}

// NewBus creates a server-wide event bus.
func NewBus() *Bus {
	return &Bus{
		subscribers: make(map[string]*Subscription),
	}
}

// Subscribe registers a named subscriber with a handler for specific event types.
// Can be called multiple times with the same name to update the subscription.
func (b *Bus) Subscribe(name string, types []apiEvents.Type, handler Handler) {
	typeSet := make(map[apiEvents.Type]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[name] = &Subscription{
		Name:    name,
		Types:   typeSet,
		Handler: handler,
	}

	logger.InfoNoCtx().Str("subscriber", name).Int("types", len(types)).Msg("event subscription registered")
}

// Unsubscribe removes a subscriber.
func (b *Bus) Unsubscribe(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, name)
}

// Emit sends an event to all interested subscribers. Non-blocking.
// Implements api/events.Emitter.
func (b *Bus) Emit(evt apiEvents.Event) {
	b.mu.RLock()
	if len(b.subscribers) == 0 {
		b.mu.RUnlock()
		return
	}

	evtType := apiEvents.Type(evt.Proto.Type)

	var targets []*Subscription
	for _, sub := range b.subscribers {
		if sub.Types[evtType] {
			targets = append(targets, sub)
		}
	}
	b.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	for _, sub := range targets {
		s := sub
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Surface the originating operation_id so the panic can be
					// correlated with the producer via Grafana/logs. No ctx is
					// available at this callsite — the bus is upstream of any
					// op lookup — so we lift op_id from the event itself.
					logger.ErrorNoCtx().Str("subscriber", s.Name).Str("event", string(evtType)).
						Str("operation_id", evt.Proto.OperationId).
						Interface("panic", r).Msg("event handler panicked")
				}
			}()
			s.Handler(evt)
		}()
	}
}

// HasSubscriber returns true if a subscriber with the given name is registered.
func (b *Bus) HasSubscriber(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.subscribers[name]
	return ok
}

// HasSubscribers returns true if any subscribers are registered. Zero-cost guard.
func (b *Bus) HasSubscribers() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers) > 0
}
