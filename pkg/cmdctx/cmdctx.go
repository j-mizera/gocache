// Package cmdctx provides per-command hook context for the GoCache plugin system.
//
// A command context is a string key-value map that flows through the hook chain:
// pre-hooks can write values, post-hooks can read them. The server injects
// built-in timing values automatically.
//
// Three namespaces control visibility:
//   - Server keys ("_" prefix): written by the server, readable by all plugins.
//   - Plugin-private keys ("<plugin>." prefix): auto-namespaced by the server,
//     visible only to the owning plugin.
//   - Shared keys ("shared." prefix): explicitly written by a plugin,
//     readable by all plugins.
package cmdctx

import "strings"

// Server-injected context keys. Set by the server on every command,
// available to all plugins.
const (
	StartNs   = "_start_ns"   // Command start timestamp (nanoseconds since epoch)
	ElapsedNs = "_elapsed_ns" // Command execution duration (nanoseconds), post-hook only
)

// SharedPrefix is the key prefix for cross-plugin shared values.
// Plugins write keys with this prefix to make them visible to all others.
// Keys without this prefix are auto-namespaced to the owning plugin.
const SharedPrefix = "shared."

// New creates an empty command context.
func New() map[string]string {
	return make(map[string]string)
}

// Merge adds context values from a plugin's hook response into the accumulated
// context. Keys are auto-prefixed with "<pluginName>." unless they already
// start with SharedPrefix.
func Merge(ctx map[string]string, pluginName string, values map[string]string) {
	for k, v := range values {
		if strings.HasPrefix(k, SharedPrefix) {
			ctx[k] = v
		} else {
			ctx[pluginName+"."+k] = v
		}
	}
}

// Filter returns the subset of the context visible to a specific plugin:
//   - All "_" prefixed keys (server context)
//   - All "<pluginName>." prefixed keys (plugin's own namespace)
//   - All "shared." prefixed keys (cross-plugin shared)
//
// Returns nil if no keys match.
func Filter(ctx map[string]string, pluginName string) map[string]string {
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
