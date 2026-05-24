package permissions

import (
	"sync"

	"gocache/api/scope"
)

// Registry stores granted scopes per plugin. Thread-safe for concurrent access.
type Registry struct {
	mu     sync.RWMutex
	scopes map[string][]scope.Scope // pluginName -> granted scopes
}

// NewRegistry creates an empty scope registry.
func NewRegistry() *Registry {
	return &Registry{
		scopes: make(map[string][]scope.Scope),
	}
}

// Register stores the granted scopes for a plugin.
func (r *Registry) Register(pluginName string, scopes []scope.Scope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopes[pluginName] = scopes
}

// Unregister removes all scope information for a plugin.
func (r *Registry) Unregister(pluginName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.scopes, pluginName)
}

// GetScopes returns the granted scopes for a plugin. Returns nil if not found.
func (r *Registry) GetScopes(pluginName string) []scope.Scope {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scopes[pluginName]
}

// HasScope returns true if the plugin has the given scope (hierarchy-aware).
func (r *Registry) HasScope(pluginName string, need scope.Scope) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.scopes[pluginName] {
		if scope.Implies(s, need) {
			return true
		}
	}
	return false
}

// KeyScopes returns only the keys:* scopes for a plugin.
func (r *Registry) KeyScopes(pluginName string) []scope.Scope {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []scope.Scope
	for _, s := range r.scopes[pluginName] {
		if scope.IsKeyScope(s) {
			result = append(result, s)
		}
	}
	return result
}
