package command

import "strings"

// Hook context constants. These are server-injected keys available to all
// plugins in the hook context map.
const (
	StartNs   = "_start_ns"   // Command start timestamp (nanoseconds since epoch)
	ElapsedNs = "_elapsed_ns" // Command execution duration (nanoseconds), post-hook only
)

// SharedPrefix is the key prefix for cross-plugin shared values.
// Plugins write keys with this prefix to make them visible to all others.
// Keys without this prefix are auto-namespaced to the owning plugin.
const SharedPrefix = "shared."

// NewHookCtx creates an empty hook context map.
func NewHookCtx() map[string]string {
	return make(map[string]string)
}

// MergeHookCtx adds context values from a plugin's hook response into the
// accumulated context. Keys are auto-prefixed with "<pluginName>." unless
// they already start with SharedPrefix.
func MergeHookCtx(ctx map[string]string, pluginName string, values map[string]string) {
	for k, v := range values {
		if strings.HasPrefix(k, SharedPrefix) {
			ctx[k] = v
		} else {
			ctx[pluginName+"."+k] = v
		}
	}
}

// FilterHookCtx returns the subset of the context visible to a specific plugin:
//   - All "_" prefixed keys (server context)
//   - All "<pluginName>." prefixed keys (plugin's own namespace)
//   - All "shared." prefixed keys (cross-plugin shared)
//
// Returns nil if no keys match.
func FilterHookCtx(ctx map[string]string, pluginName string) map[string]string {
	if len(ctx) == 0 {
		return nil
	}
	prefix := pluginName + "."
	filtered := make(map[string]string)
	for k, v := range ctx {
		if strings.HasPrefix(k, "_") || strings.HasPrefix(k, prefix) || strings.HasPrefix(k, SharedPrefix) {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}
