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

// Complete marks the operation as completed and removes it from active.
func (t *Tracker) Complete(id string) {
	t.mu.Lock()
	if op, ok := t.active[id]; ok {
		op.Complete()
		delete(t.active, id)
	}
	t.mu.Unlock()
}

// Fail marks the operation as failed and removes it from active.
func (t *Tracker) Fail(id string, reason string) {
	t.mu.Lock()
	if op, ok := t.active[id]; ok {
		op.Fail(reason)
		delete(t.active, id)
	}
	t.mu.Unlock()
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
