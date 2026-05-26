package plugin

import (
	"time"
)

// Failure policy values for PluginOverride.FailurePolicy.
const (
	// FailurePolicyContinue (or the empty string) means a plugin failure
	// does NOT halt the server. Non-critical plugins restart up to
	// MaxRestarts attempts before being marked shut down. Default.
	FailurePolicyContinue = "continue"
	// FailurePolicyHaltServer means a plugin process crash or start
	// failure causes the server to exit fatally. Use sparingly — only
	// for plugins the server cannot correctly serve traffic without
	// (typically auth, rate limiting, compliance filters).
	FailurePolicyHaltServer = "halt_server"
)

// PluginsConfig holds the plugin system configuration.
type PluginsConfig struct {
	Enabled         bool          `yaml:"enabled"          mapstructure:"enabled"`
	Dir             string        `yaml:"dir"              mapstructure:"dir"`
	SocketPath      string        `yaml:"socket_path"      mapstructure:"socket_path"`
	HealthInterval  time.Duration `yaml:"health_interval"  mapstructure:"health_interval"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" mapstructure:"shutdown_timeout"`
	MaxRestarts     int           `yaml:"max_restarts"     mapstructure:"max_restarts"`
	ConnectTimeout  time.Duration `yaml:"connect_timeout"  mapstructure:"connect_timeout"`
	// MinRestartIntervalForReplay is the grace window during which a
	// re-registering plugin is treated as "already caught up" and gets no
	// operation-hook replay. A crash-looping plugin would otherwise see a
	// full ring of synthetic PhaseStart events on every restart, stalling
	// its own reconnect and drowning downstream observability. Zero
	// disables suppression (every Register fires a full replay).
	MinRestartIntervalForReplay time.Duration             `yaml:"min_restart_interval_for_replay" mapstructure:"min_restart_interval_for_replay"`
	Overrides                   map[string]PluginOverride `yaml:"overrides"                       mapstructure:"overrides"`
}

// PluginOverride allows YAML to override plugin self-described properties.
//
// FailurePolicy ("continue" or "halt_server") is the canonical halt-on-fail
// flag. The legacy `critical: true` boolean is still accepted for one
// release and maps to FailurePolicyHaltServer; a warning is logged the
// first time it is observed. Migrate new configs to FailurePolicy.
type PluginOverride struct {
	Binary        string   `yaml:"binary"         mapstructure:"binary"`
	FailurePolicy string   `yaml:"failure_policy" mapstructure:"failure_policy"`
	Critical      bool     `yaml:"critical"       mapstructure:"critical"` // Deprecated: use FailurePolicy.
	Priority      int      `yaml:"priority"       mapstructure:"priority"`
	Scopes        []string `yaml:"scopes"         mapstructure:"scopes"`
}

// IsCritical reports whether the plugin should halt the server on failure.
// FailurePolicy is the canonical field; when set to a known value it wins
// outright. Unknown values + the unset case fall back to the legacy
// Critical bool so existing configs keep working through the migration
// window. Deprecation/unknown-value warnings are emitted by the caller
// (pkg/plugin) which has access to the logger.
func (o PluginOverride) IsCritical() bool {
	switch o.FailurePolicy {
	case FailurePolicyHaltServer:
		return true
	case FailurePolicyContinue:
		return false
	}
	return o.Critical
}
