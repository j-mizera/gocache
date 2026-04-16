// Package operations defines the Operation model for GoCache.
//
// An Operation is a unit of work with identity, timing, parent-child
// relationships, and a security-aware context bag. Operations form a tree
// and are the foundation for distributed tracing, log correlation, and
// plugin context enrichment.
//
// This package lives in api/ — both the plugin SDK and server import it.
package operations

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	opctx "gocache/api/context"
)

// Type identifies the kind of operation.
type Type string

const (
	TypeCommand      Type = "command"
	TypeConnection   Type = "connection"
	TypeCleanup      Type = "cleanup"
	TypeSnapshot     Type = "snapshot"
	TypeStartup      Type = "startup"
	TypeShutdown     Type = "shutdown"
	TypeConfigReload Type = "config_reload"
	TypePluginStart  Type = "plugin_start"
	TypePluginStop   Type = "plugin_stop"
)

// Short prefixes for human-readable IDs.
var typePrefixes = map[Type]string{
	TypeCommand:      "cmd",
	TypeConnection:   "conn",
	TypeCleanup:      "cleanup",
	TypeSnapshot:     "snap",
	TypeStartup:      "boot",
	TypeShutdown:     "shut",
	TypeConfigReload: "cfg",
	TypePluginStart:  "pstart",
	TypePluginStop:   "pstop",
}

// Status represents the lifecycle state of an operation.
type Status int

const (
	StatusRunning Status = iota
	StatusCompleted
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// ID generation: per-type atomic counter.
var idCounters sync.Map // Type -> *atomic.Uint64

func nextID(t Type) string {
	v, _ := idCounters.LoadOrStore(t, &atomic.Uint64{})
	counter := v.(*atomic.Uint64)
	prefix := typePrefixes[t]
	if prefix == "" {
		prefix = string(t)
	}
	return fmt.Sprintf("%s_%d", prefix, counter.Add(1))
}

// Operation represents a unit of work in the server.
type Operation struct {
	ID         string
	Type       Type
	ParentID   string // empty for root operations
	StartTime  time.Time
	EndTime    time.Time // zero until completed/failed
	Status     Status
	FailReason string
	Context    map[string]string // 4-tier security-aware context bag
	mu         sync.RWMutex
}

// New creates a running operation with a unique ID and empty context.
func New(opType Type, parentID string) *Operation {
	return &Operation{
		ID:        nextID(opType),
		Type:      opType,
		ParentID:  parentID,
		StartTime: time.Now(),
		Status:    StatusRunning,
		Context:   opctx.NewContext(),
	}
}

// Enrich adds a single key-value to the context. Thread-safe.
func (op *Operation) Enrich(key, value string) {
	op.mu.Lock()
	op.Context[key] = value
	op.mu.Unlock()
}

// EnrichMany adds multiple key-values to the context. Thread-safe.
func (op *Operation) EnrichMany(values map[string]string) {
	if len(values) == 0 {
		return
	}
	op.mu.Lock()
	for k, v := range values {
		op.Context[k] = v
	}
	op.mu.Unlock()
}

// Get reads a single key from the context. Thread-safe.
func (op *Operation) Get(key string) (string, bool) {
	op.mu.RLock()
	defer op.mu.RUnlock()
	v, ok := op.Context[key]
	return v, ok
}

// ContextSnapshot returns a copy of the context.
// If redacted is true, secret keys are stripped.
func (op *Operation) ContextSnapshot(redacted bool) map[string]string {
	op.mu.RLock()
	cp := make(map[string]string, len(op.Context))
	for k, v := range op.Context {
		cp[k] = v
	}
	op.mu.RUnlock()
	if redacted {
		return opctx.RedactSecrets(cp)
	}
	return cp
}

// FilteredContext returns a visibility-filtered context snapshot for a plugin.
// If redacted is true, secret keys are also stripped.
func (op *Operation) FilteredContext(pluginName string, redacted bool) map[string]string {
	snapshot := op.ContextSnapshot(redacted)
	return opctx.FilterForPlugin(snapshot, pluginName)
}

// Complete marks the operation as completed and sets EndTime.
func (op *Operation) Complete() {
	op.mu.Lock()
	op.Status = StatusCompleted
	op.EndTime = time.Now()
	op.mu.Unlock()
}

// Fail marks the operation as failed with a reason.
func (op *Operation) Fail(reason string) {
	op.mu.Lock()
	op.Status = StatusFailed
	op.EndTime = time.Now()
	op.FailReason = reason
	op.mu.Unlock()
}

// Duration returns the elapsed time. If still running, measures from start to now.
func (op *Operation) Duration() time.Duration {
	op.mu.RLock()
	defer op.mu.RUnlock()
	if op.EndTime.IsZero() {
		return time.Since(op.StartTime)
	}
	return op.EndTime.Sub(op.StartTime)
}
