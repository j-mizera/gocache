package config

import "time"

// PluginConfig is the typed read-only view of a single embedded plugin's
// configuration subsection. The server hands one to each plugin's
// SnapshotProvider.Build (and re-hands a fresh view on every config
// reload via ReloadHandler) — keys are scoped to the plugin's own
// namespace, so the plugin reads "file" rather than the full path.
//
// PluginConfig is intentionally library-agnostic. The server's choice
// of underlying configuration store (viper today, anything else
// tomorrow) lives behind this interface; plugins import only api/* and
// never name the implementation library.
//
// SetDefault is the one mutating method on the interface. It registers
// a fallback for key — Get* returns this value when the operator hasn't
// set the key in YAML / env / flags. Optional: a plugin that does not
// call SetDefault for a key it reads is declaring the key required, and
// its Build should return an error when Get* comes back empty.
//
// See ADR-0008 for the rationale.
type PluginConfig interface {
	GetString(key string) string
	GetInt(key string) int
	GetInt64(key string) int64
	GetBool(key string) bool
	GetDuration(key string) time.Duration
	GetStringSlice(key string) []string
	IsSet(key string) bool
	SetDefault(key string, value any)
}

// ReloadHandler is the optional capability a plugin's Provider
// implements to react to runtime config-file reloads. The server
// type-asserts on this after Build returns; a Provider that does not
// implement it skips reload entirely — which is the right default for
// plugins with no runtime-tunable knobs.
//
// The callback runs on the server's reload goroutine. Implementations
// should be quick and avoid blocking on I/O; the typical pattern is
// "re-read my keys, swap an atomic pointer or call SetX on the live
// Source/Sink/Snapshotter".
type ReloadHandler interface {
	OnConfigReload(cfg PluginConfig)
}
