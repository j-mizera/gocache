package permissions

import (
	"fmt"

	"gocache/api/scope"
)

// Enforcer performs runtime scope checks before plugin operations.
type Enforcer struct {
	registry *Registry
}

// NewEnforcer creates a scope enforcer backed by the given registry.
func NewEnforcer(r *Registry) *Enforcer {
	return &Enforcer{registry: r}
}

// Check verifies that the plugin has the required scope for the operation
// and that all keys fall within the plugin's allowed key namespaces.
// Returns nil if authorized, or a descriptive error.
func (e *Enforcer) Check(pluginName string, op scope.OpType, keys []string) error {
	required := scope.RequiredScope(op)
	if !e.registry.HasScope(pluginName, required) {
		return fmt.Errorf("plugin %q lacks scope %q", pluginName, required)
	}

	keyScopes := e.registry.KeyScopes(pluginName)
	if len(keyScopes) == 0 {
		return nil
	}

	for _, key := range keys {
		if key == "" {
			continue
		}
		if !matchesAnyKey(keyScopes, key) {
			return fmt.Errorf("plugin %q cannot access key %q (allowed patterns: %v)", pluginName, key, scope.ScopeStrings(keyScopes))
		}
	}

	return nil
}

func matchesAnyKey(keyScopes []scope.Scope, key string) bool {
	for _, s := range keyScopes {
		if scope.MatchesKey(s, key) {
			return true
		}
	}
	return false
}
