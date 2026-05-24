package config

import "time"

// PluginConfig is the typed read-only view of a single embedded plugin's
// configuration subsection. The server hands one to each plugin's
// Provider.Build (and re-hands a fresh view on every config reload via
// ReloadHandler) — keys are scoped to the plugin's own namespace, so
// the plugin reads "file" rather than the full path.
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

	// BindEnv maps a plugin key to one or more env var names. The first
	// matching env var wins. The long-form GOCACHE_PLUGINS_CONFIG_<NAME>_KEY
	// continues to work via the config loader's automatic env resolution;
	// BindEnv adds shorter aliases (e.g. GOCACHE_AOF_FILE). Call this in
	// Build before reading keys. See ADR-0018.
	BindEnv(key string, envVars ...string)

	// MergeFile reads a config file (YAML, JSON, or TOML — format is
	// auto-detected from the file extension) and merges its keys into the
	// plugin's namespace as low-priority values.
	// Priority: env > server config > plugin file > SetDefault.
	// A missing file is silently ignored (returns nil). A parse error is
	// returned. See ADR-0018.
	MergeFile(path string) error
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
