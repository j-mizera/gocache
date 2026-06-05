package telemetrytest

import (
	"testing"
	"time"

	apievents "gocache/api/events"
)

const (
	defaultWaitTimeout  = 2 * time.Second
	defaultPollInterval = time.Millisecond
)

// DrainWorker is the narrow part of a telemetry drain worker needed by tests.
// Production packages can satisfy this with their own worker type without this
// common test helper importing server internals.
type DrainWorker interface {
	DrainOnce() int
}

// EventWaiter waits for telemetry events materialized by a drain worker while
// documenting the important OperationTracker rule: public telemetry is projected
// only after the operation that owns the record is completed and drained. Tests
// for facts emitted during long-lived operations should not force the long-lived
// operation to finish just to make assertions pass; production code should
// record visible facts on short completed operations, and tests should drain
// completed operations while waiting.
type EventWaiter struct {
	t       testing.TB
	worker  DrainWorker
	events  <-chan apievents.Event
	pending []apievents.Event
	timeout time.Duration
	poll    time.Duration
}

// NewEventWaiter returns a waiter that drains completed telemetry operations
// while waiting for events. Unmatched events are retained for later Wait calls,
// so tests can assert multiple event types even when the drain worker emits them
// in a different order than the test checks them.
func NewEventWaiter(t testing.TB, worker DrainWorker, events <-chan apievents.Event) *EventWaiter {
	t.Helper()
	return &EventWaiter{
		t:       t,
		worker:  worker,
		events:  events,
		timeout: defaultWaitTimeout,
		poll:    defaultPollInterval,
	}
}

// Wait returns the first event matching match, failing the test if it does not
// appear before the waiter timeout. It performs a drain on every poll so tests
// do not depend on the background drain ticker timing.
func (w *EventWaiter) Wait(description string, match func(apievents.Event) bool) apievents.Event {
	w.t.Helper()
	deadline := time.NewTimer(w.timeout)
	defer deadline.Stop()
	tick := time.NewTicker(w.poll)
	defer tick.Stop()
	for {
		for i, event := range w.pending {
			if match(event) {
				w.pending = append(w.pending[:i], w.pending[i+1:]...)
				return event
			}
		}
		if w.worker != nil {
			w.worker.DrainOnce()
		}
		select {
		case event := <-w.events:
			if match(event) {
				return event
			}
			w.pending = append(w.pending, event)
		case <-tick.C:
		case <-deadline.C:
			w.t.Fatalf("timed out waiting for %s", description)
		}
	}
}
