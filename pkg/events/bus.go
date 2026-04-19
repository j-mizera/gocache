// Package events implements the server-wide event bus.
//
// Any server component can emit and subscribe to events. The plugin system
// bridges the bus to GCPC for remote plugin delivery, but the bus itself
// is not plugin-specific.
//
// The bus retains a bounded ring of recent events so subscribers that
// attach after boot (for example an IPC observability plugin coming online
// at t=500ms) can still observe events emitted from t=0. See ring.go.
package events

import (
	"sync"

	apiEvents "gocache/api/events"
	"gocache/api/logger"
)

// DefaultReplayCapacity is the ring size used when callers do not provide
// one. Sized for ~5 MB at 500 B average event payload — enough to cover
// the full boot sequence of a typical deployment without unbounded growth.
const DefaultReplayCapacity = 10_000

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
	ring        *ring
}

// NewBus creates a server-wide event bus with the default replay capacity.
func NewBus() *Bus {
	return NewBusWithCapacity(DefaultReplayCapacity)
}

// NewBusWithCapacity creates a bus whose replay ring holds up to capacity
// events. capacity<=0 disables replay entirely — useful for tests or
// deployments that want zero retention overhead.
func NewBusWithCapacity(capacity int) *Bus {
	return &Bus{
		subscribers: make(map[string]*Subscription),
		ring:        newRing(capacity),
	}
}

// Subscribe registers a named subscriber with a handler for specific event
// types. Retained events that match the subscriber's type filter are
// replayed in FIFO order before the call returns; any events emitted after
// Subscribe returns are delivered live. Handlers must remain non-blocking
// because replay runs synchronously on the caller's goroutine.
//
// Can be called multiple times with the same name to update the subscription.
// In that case nothing is replayed — the subscriber is assumed to already
// be caught up.
func (b *Bus) Subscribe(name string, types []apiEvents.Type, handler Handler) {
	typeSet := make(map[apiEvents.Type]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}

	b.mu.Lock()
	_, existed := b.subscribers[name]
	b.subscribers[name] = &Subscription{
		Name:    name,
		Types:   typeSet,
		Handler: handler,
	}
	// Snapshot under the same lock that gates Emit — any concurrent Emit
	// either committed to the ring before us (and is in the snapshot) or
	// runs after Subscribe returns (and is delivered live). No dup, no gap.
	var replay []apiEvents.Event
	var dropped uint64
	if !existed {
		replay, dropped = b.ring.snapshot()
	}
	b.mu.Unlock()

	logger.InfoNoCtx().
		Str("subscriber", name).
		Int("types", len(types)).
		Int("replay_events", len(replay)).
		Uint64("replay_dropped", dropped).
		Msg("event subscription registered")

	if existed {
		return
	}

	// ReplayGap is surfaced before replay so a subscriber that alerts on
	// gaps sees the marker at the position in its inbox where the gap
	// actually occurred — i.e. immediately before the oldest retained event.
	if dropped > 0 && typeSet[apiEvents.ReplayGap] {
		deliverOne(name, handler, apiEvents.NewReplayGap(name, dropped))
	}
	for _, evt := range replay {
		if typeSet[apiEvents.Type(evt.Proto.Type)] {
			deliverOne(name, handler, evt)
		}
	}
}

// Unsubscribe removes a subscriber.
func (b *Bus) Unsubscribe(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, name)
}

// Emit sends an event to all interested subscribers and records it in the
// replay ring. Non-blocking. Implements api/events.Emitter.
func (b *Bus) Emit(evt apiEvents.Event) {
	b.mu.Lock()
	b.ring.push(evt)
	if len(b.subscribers) == 0 {
		b.mu.Unlock()
		return
	}

	evtType := apiEvents.Type(evt.Proto.Type)

	var targets []*Subscription
	for _, sub := range b.subscribers {
		if sub.Types[evtType] {
			targets = append(targets, sub)
		}
	}
	b.mu.Unlock()

	for _, sub := range targets {
		deliverOne(sub.Name, sub.Handler, evt)
	}
}

// deliverOne invokes handler with panic isolation so a single bad
// subscriber cannot take down the emitter.
func deliverOne(name string, handler Handler, evt apiEvents.Event) {
	defer func() {
		if r := recover(); r != nil {
			// Surface the originating operation_id so the panic can be
			// correlated with the producer via Grafana/logs. No ctx is
			// available at this callsite — the bus is upstream of any
			// op lookup — so we lift op_id from the event itself.
			logger.ErrorNoCtx().
				Str("subscriber", name).
				Str("event", evt.Proto.Type).
				Str("operation_id", evt.Proto.OperationId).
				Interface("panic", r).
				Msg("event handler panicked")
		}
	}()
	handler(evt)
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
