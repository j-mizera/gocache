// Package operations provides the server-side operation tracker.
//
// The Tracker maintains the set of active operations and provides
// lifecycle management (start, complete, fail) and introspection.
package operations

import (
	"sync"

	ops "gocache/api/operations"
)

// Tracker maintains the registry of active operations.
// Thread-safe for concurrent access from multiple goroutines.
type Tracker struct {
	mu     sync.RWMutex
	active map[string]*ops.Operation
}

// NewTracker creates a new operation tracker.
func NewTracker() *Tracker {
	return &Tracker{
		active: make(map[string]*ops.Operation),
	}
}

// Start creates a new operation, registers it, and returns it.
func (t *Tracker) Start(opType ops.Type, parentID string) *ops.Operation {
	op := ops.New(opType, parentID)
	t.mu.Lock()
	t.active[op.ID] = op
	t.mu.Unlock()
	return op
}

// finish applies finalize to the op with id and removes it from active.
// No-op if the id is unknown (already completed or never registered).
func (t *Tracker) finish(id string, finalize func(*ops.Operation)) {
	t.mu.Lock()
	if op, ok := t.active[id]; ok {
		finalize(op)
		delete(t.active, id)
	}
	t.mu.Unlock()
}

// Complete marks the operation as completed and removes it from active.
func (t *Tracker) Complete(id string) {
	t.finish(id, (*ops.Operation).Complete)
}

// Fail marks the operation as failed and removes it from active.
func (t *Tracker) Fail(id string, reason string) {
	t.finish(id, func(op *ops.Operation) { op.Fail(reason) })
}

// Get returns an active operation by ID. Returns nil if not found or already completed.
func (t *Tracker) Get(id string) *ops.Operation {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.active[id]
}

// Active returns a snapshot of all active operations.
func (t *Tracker) Active() []*ops.Operation {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]*ops.Operation, 0, len(t.active))
	for _, op := range t.active {
		result = append(result, op)
	}
	return result
}

// ActiveCount returns the number of active operations.
func (t *Tracker) ActiveCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.active)
}
