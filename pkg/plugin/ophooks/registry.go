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

// Registry manages operation hook registrations.
type Registry struct {
	mu    sync.RWMutex
	hooks []*HookEntry // sorted by priority
}

// NewRegistry creates an empty operation hook registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds operation hooks for a plugin.
func (r *Registry) Register(pluginName string, priority int, conn *router.PluginConn, patterns []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range patterns {
		r.hooks = append(r.hooks, &HookEntry{
			PluginName: pluginName,
			Pattern:    strings.ToLower(p),
			Priority:   priority,
			Conn:       conn,
		})
	}
	sort.Slice(r.hooks, func(i, j int) bool {
		return r.hooks[i].Priority < r.hooks[j].Priority
	})
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
