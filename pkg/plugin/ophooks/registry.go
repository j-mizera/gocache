// Package ophooks implements the operation hook registry and executor.
//
// Operation hooks are the GoCache equivalent of Linux kernel netfilter hooks.
// Plugins register as operation context providers and are called synchronously
// when an operation starts, enriching its context before any work begins.
package ophooks

import (
	"sort"
	"strings"
	"sync"
	"time"

	ops "gocache/api/operations"
	"gocache/pkg/plugin/router"
)

// HookEntry represents a registered operation hook.
type HookEntry struct {
	PluginName string
	Pattern    string // operation type to match, "*" for all
	Priority   int    // lower = fires first
	Conn       *router.PluginConn
}

// RegisterCallback runs after a plugin's hooks are committed to the
// registry. regTime is the moment registration completed — callers can
// use it as a watermark so ops that started before this time can be
// replayed as synthetic PhaseStart requests without duplicating the
// live hook that fires for ops starting after.
type RegisterCallback func(pluginName string, regTime time.Time)

// Registry manages operation hook registrations.
type Registry struct {
	mu           sync.RWMutex
	hooks        []*HookEntry // sorted by priority
	onRegister   RegisterCallback
	onRegisterMu sync.RWMutex // separate so callbacks can't deadlock with dispatch
}

// NewRegistry creates an empty operation hook registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// SetOnRegister installs a callback invoked after each successful Register.
// Wiring happens in main.go so the executor (which needs tracker +
// process-start-time) drives replay without the registry depending on it.
func (r *Registry) SetOnRegister(fn RegisterCallback) {
	r.onRegisterMu.Lock()
	r.onRegister = fn
	r.onRegisterMu.Unlock()
}

// Register adds operation hooks for a plugin and fires the optional
// onRegister callback with the moment registration became visible. The
// callback runs outside the registry write lock so replay machinery can
// consult other locks (tracker, executor) without risking deadlock.
func (r *Registry) Register(pluginName string, priority int, conn *router.PluginConn, patterns []string) {
	r.mu.Lock()
	for _, p := range patterns {
		r.hooks = append(r.hooks, &HookEntry{
			PluginName: pluginName,
			Pattern:    strings.ToLower(p),
			Priority:   priority,
			Conn:       conn,
		})
	}
	// Stable so equal priorities keep registration order — matches cmdhooks.
	sort.SliceStable(r.hooks, func(i, j int) bool {
		return r.hooks[i].Priority < r.hooks[j].Priority
	})
	// Capture the watermark inside the lock so any concurrent Match that
	// sees this plugin necessarily happens at or after regTime.
	regTime := time.Now()
	r.mu.Unlock()

	r.onRegisterMu.RLock()
	cb := r.onRegister
	r.onRegisterMu.RUnlock()
	if cb != nil {
		cb(pluginName, regTime)
	}
}

// PatternsFor returns the operation-type patterns registered for a plugin.
// Used by the executor to filter replay to the plugin's declared interest.
func (r *Registry) PatternsFor(pluginName string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, h := range r.hooks {
		if h.PluginName == pluginName {
			out = append(out, h.Pattern)
		}
	}
	return out
}

// ConnFor returns the first connection registered for a plugin (they all
// share the same PluginConn). nil if the plugin has no ophooks registered.
func (r *Registry) ConnFor(pluginName string) *router.PluginConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.hooks {
		if h.PluginName == pluginName {
			return h.Conn
		}
	}
	return nil
}

// Unregister removes all hooks for a plugin.
func (r *Registry) Unregister(pluginName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	filtered := r.hooks[:0]
	for _, h := range r.hooks {
		if h.PluginName != pluginName {
			filtered = append(filtered, h)
		}
	}
	r.hooks = filtered
}

// Match returns all hooks that match an operation type, in priority order.
func (r *Registry) Match(opType ops.Type) []*HookEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*HookEntry
	t := strings.ToLower(string(opType))
	for _, h := range r.hooks {
		if h.Pattern == "*" || h.Pattern == t {
			result = append(result, h)
		}
	}
	return result
}

// HasAny returns true if any hooks are registered.
func (r *Registry) HasAny() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.hooks) > 0
}
