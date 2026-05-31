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
	"sync/atomic"

	apiconfig "gocache/api/config"
	apiEvents "gocache/api/events"
	"gocache/commons/logger"
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
	typeCounts  map[apiEvents.Type]int
	typeSubs    map[apiEvents.Type]map[string]*Subscription
	ring        *ring

	// subCount mirrors len(subscribers) for the lock-free HasSubscribers
	// check on the evaluator hot path. Updated under mu so it stays in
	// sync with the map; read with atomic.Load so callers don't pay the
	// lock cost on every command. The eventual-consistency window is
	// ≤ a single Subscribe/Unsubscribe call.
	subCount atomic.Int32

	// interestMask mirrors typeCounts for known event types. It lets command
	// producers ask whether a specific event type has subscribers before they
	// allocate protobuf payloads or context snapshots.
	interestMask atomic.Uint64
}

// NewBus creates a server-wide event bus with the default replay capacity.
func NewBus() *Bus {
	return NewBusWithCapacity(apiconfig.DefaultEventsReplayCapacity)
}

// NewBusWithCapacity creates a bus whose replay ring holds up to capacity
// events. capacity<=0 disables replay entirely — useful for tests or
// deployments that want zero retention overhead.
func NewBusWithCapacity(capacity int) *Bus {
	return &Bus{
		subscribers: make(map[string]*Subscription),
		typeCounts:  make(map[apiEvents.Type]int),
		typeSubs:    make(map[apiEvents.Type]map[string]*Subscription),
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
	previous, existed := b.subscribers[name]
	if existed {
		b.removeTypeSubscribersLocked(previous)
		b.applyTypeDeltaLocked(previous.Types, -1)
	}
	sub := &Subscription{
		Name:    name,
		Types:   typeSet,
		Handler: handler,
	}
	b.subscribers[name] = sub
	b.addTypeSubscribersLocked(sub)
	b.applyTypeDeltaLocked(typeSet, 1)
	if !existed {
		b.subCount.Add(1)
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
	if sub, ok := b.subscribers[name]; ok {
		b.removeTypeSubscribersLocked(sub)
		b.applyTypeDeltaLocked(sub.Types, -1)
		delete(b.subscribers, name)
		b.subCount.Add(-1)
	}
	b.mu.Unlock()
}

// Emit sends an event to all interested subscribers and records it in the
// replay ring. Non-blocking. Implements api/events.Emitter.
func (b *Bus) Emit(evt apiEvents.Event) {
	b.mu.Lock()
	b.ring.push(evt)
	evtType := apiEvents.Type(evt.Proto.Type)
	typeSubs := b.typeSubs[evtType]
	if len(typeSubs) == 0 {
		b.mu.Unlock()
		return
	}

	targets := make([]*Subscription, 0, len(typeSubs))
	for _, sub := range typeSubs {
		targets = append(targets, sub)
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

// HasSubscribers returns true if any subscribers are registered. Implemented
// as an atomic load so the evaluator hot path can call it on every command
// without paying RLock acquisition cost. Stays consistent with
// len(subscribers) because the counter is mutated under the same write lock
// that mutates the map.
func (b *Bus) HasSubscribers() bool {
	return b.subCount.Load() > 0
}

// HasSubscribersFor reports whether any subscriber is interested in at least
// one of the given event types. Known event types are checked with a lock-free
// bitmask so command producers can skip event-specific payload construction.
func (b *Bus) HasSubscribersFor(types ...apiEvents.Type) bool {
	mask := b.interestMask.Load()
	if mask == 0 {
		return false
	}
	for _, eventType := range types {
		bit := eventTypeBit(eventType)
		if bit == 0 {
			return b.HasSubscribers()
		}
		if mask&bit != 0 {
			return true
		}
	}
	return false
}

func (b *Bus) addTypeSubscribersLocked(sub *Subscription) {
	for eventType := range sub.Types {
		if b.typeSubs[eventType] == nil {
			b.typeSubs[eventType] = make(map[string]*Subscription)
		}
		b.typeSubs[eventType][sub.Name] = sub
	}
}

func (b *Bus) removeTypeSubscribersLocked(sub *Subscription) {
	for eventType := range sub.Types {
		typeSubs := b.typeSubs[eventType]
		if len(typeSubs) == 0 {
			continue
		}
		delete(typeSubs, sub.Name)
		if len(typeSubs) == 0 {
			delete(b.typeSubs, eventType)
		}
	}
}

func (b *Bus) applyTypeDeltaLocked(types map[apiEvents.Type]bool, delta int) {
	if len(types) == 0 || delta == 0 {
		return
	}
	mask := b.interestMask.Load()
	for eventType := range types {
		count := b.typeCounts[eventType] + delta
		if count <= 0 {
			delete(b.typeCounts, eventType)
			mask &^= eventTypeBit(eventType)
			continue
		}
		b.typeCounts[eventType] = count
		mask |= eventTypeBit(eventType)
	}
	b.interestMask.Store(mask)
}

func eventTypeBit(eventType apiEvents.Type) uint64 {
	switch eventType {
	case apiEvents.CommandStarted:
		return 1 << 0
	case apiEvents.CommandCompleted:
		return 1 << 1
	case apiEvents.ConnectionOpen:
		return 1 << 2
	case apiEvents.ConnectionClose:
		return 1 << 3
	case apiEvents.ServerStart:
		return 1 << 4
	case apiEvents.ServerShutdown:
		return 1 << 5
	case apiEvents.PluginRegistered:
		return 1 << 6
	case apiEvents.PluginCrashed:
		return 1 << 7
	case apiEvents.PluginRestarted:
		return 1 << 8
	case apiEvents.ConfigReloaded:
		return 1 << 9
	case apiEvents.AuthFailed:
		return 1 << 10
	case apiEvents.CacheEviction:
		return 1 << 11
	case apiEvents.LogEntry:
		return 1 << 12
	case apiEvents.OperationStarted:
		return 1 << 13
	case apiEvents.OperationCompleted:
		return 1 << 14
	case apiEvents.ReplayGap:
		return 1 << 15
	default:
		return 0
	}
}
