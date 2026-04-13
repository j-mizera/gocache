// Package context provides unified context utilities for GoCache.
//
// The context bag is a map[string]string with a 4-tier security model:
//   - Server keys ("_" prefix): written by server, visible to all plugins
//   - Plugin-private keys ("<plugin>." prefix): visible only to the owning plugin
//   - Shared keys ("shared." prefix): visible to all plugins
//   - Secret marker (".secret." infix or "secret." prefix): never exported to telemetry
//
// This package lives in api/ — both the plugin SDK and server import it.
package context

import "strings"

// Well-known prefixes.
const (
	ServerPrefix = "_"
	SharedPrefix = "shared."
	SecretInfix  = ".secret."
	SecretPrefix = "secret."
)

// IsSecret returns true if the key is marked as a secret.
// A key is secret if any dot-separated segment is "secret":
//
//	_secret.X, plugin.secret.X, shared.secret.X, secret.X
//
// Secret values must NEVER be exported to external telemetry systems.
func IsSecret(key string) bool {
	// Fast path: check common patterns.
	if strings.HasPrefix(key, SecretPrefix) {
		return true
	}
	if strings.Contains(key, SecretInfix) {
		return true
	}
	// Handle _secret.X pattern (underscore prefix before "secret.").
	if strings.HasPrefix(key, "_secret.") {
		return true
	}
	return false
}

// FilterForPlugin returns the subset of ctx visible to pluginName:
//   - All "_*" keys (server context)
//   - All "<pluginName>.*" keys (own namespace)
//   - All "shared.*" keys (cross-plugin)
//
// Returns nil if no keys match.
func FilterForPlugin(ctx map[string]string, pluginName string) map[string]string {
	if len(ctx) == 0 {
		return nil
	}
	prefix := pluginName + "."
	filtered := make(map[string]string)
	for k, v := range ctx {
		if strings.HasPrefix(k, ServerPrefix) ||
			strings.HasPrefix(k, prefix) ||
			strings.HasPrefix(k, SharedPrefix) {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// MergeFromPlugin merges values written by a plugin into ctx.
// Keys with "shared." prefix are kept as-is.
// All other keys are auto-prefixed with "<pluginName>.".
func MergeFromPlugin(ctx map[string]string, pluginName string, values map[string]string) {
	for k, v := range values {
		if strings.HasPrefix(k, SharedPrefix) {
			ctx[k] = v
		} else {
			ctx[pluginName+"."+k] = v
		}
	}
}

// RedactSecrets returns a copy of ctx with all secret keys removed.
// Used by exporting plugins (OTEL, Kafka, audit) before sending data externally.
func RedactSecrets(ctx map[string]string) map[string]string {
	if len(ctx) == 0 {
		return nil
	}
	clean := make(map[string]string, len(ctx))
	for k, v := range ctx {
		if !IsSecret(k) {
			clean[k] = v
		}
	}
	return clean
}

// NewContext creates an empty context map.
func NewContext() map[string]string {
	return make(map[string]string)
}
