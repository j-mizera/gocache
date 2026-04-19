package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gocache/api/logger"
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

// deprecatedCriticalWarnOnce ensures we only log the migration warning
// once per process no matter how many overrides still use the old field.
// Intentional tradeoff: on a config hot-reload that re-adds a
// `critical: true` after the first warn, the second reload will stay
// silent. We accept that because the first log is sufficient — operators
// fix the YAML once, not per reload — and spamming WARN per reload would
// add noise to a hot-reload's already-busy log window.
var deprecatedCriticalWarnOnce sync.Once

// PluginsConfig holds the plugin system configuration.
type PluginsConfig struct {
	Enabled         bool                      `yaml:"enabled"          mapstructure:"enabled"`
	Dir             string                    `yaml:"dir"              mapstructure:"dir"`
	SocketPath      string                    `yaml:"socket_path"      mapstructure:"socket_path"`
	HealthInterval  time.Duration             `yaml:"health_interval"  mapstructure:"health_interval"`
	ShutdownTimeout time.Duration             `yaml:"shutdown_timeout" mapstructure:"shutdown_timeout"`
	MaxRestarts     int                       `yaml:"max_restarts"     mapstructure:"max_restarts"`
	ConnectTimeout  time.Duration             `yaml:"connect_timeout"  mapstructure:"connect_timeout"`
	Overrides       map[string]PluginOverride `yaml:"overrides"        mapstructure:"overrides"`
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
// Critical bool (with a one-shot deprecation warning on first observation),
// so existing configs keep working through the migration window.
func (o PluginOverride) IsCritical() bool {
	switch o.FailurePolicy {
	case FailurePolicyHaltServer:
		return true
	case FailurePolicyContinue:
		return false
	case "":
		// Unset — defer to legacy below.
	default:
		logger.WarnNoCtx().Str("failure_policy", o.FailurePolicy).
			Msg("unknown plugin failure_policy — falling back to legacy 'critical' field")
	}
	if o.Critical {
		deprecatedCriticalWarnOnce.Do(func() {
			logger.WarnNoCtx().Msg("plugin override uses deprecated 'critical: true' — migrate to 'failure_policy: halt_server'")
		})
		return true
	}
	return false
}

// executableBits is the mask of the owner, group, and other execute bits
// on a Unix file mode. A plugin binary must have at least one of these
// set to be considered executable by Discover.
const executableBits os.FileMode = 0111

// PluginEntry represents a discovered plugin before it connects.
type PluginEntry struct {
	Name     string
	BinPath  string
	Critical bool
	Priority int
}

// Discover scans the plugin directory for executable binaries and applies
// any YAML overrides. Returns an empty slice if the directory does not exist.
func Discover(cfg PluginsConfig) ([]*PluginEntry, error) {
	if cfg.Dir == "" {
		return nil, nil
	}

	info, err := os.Stat(cfg.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat plugin dir %s: %w", cfg.Dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plugin path %s is not a directory", cfg.Dir)
	}

	entries, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("read plugin dir %s: %w", cfg.Dir, err)
	}

	var plugins []*PluginEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		// Check if file is executable.
		if fi.Mode()&executableBits == 0 {
			continue
		}

		name := e.Name()
		entry := &PluginEntry{
			Name:    name,
			BinPath: filepath.Join(cfg.Dir, name),
		}

		// Apply YAML overrides if present.
		if override, ok := cfg.Overrides[name]; ok {
			if override.Binary != "" {
				entry.BinPath = override.Binary
			}
			entry.Critical = override.IsCritical()
			entry.Priority = override.Priority
		}

		plugins = append(plugins, entry)
	}

	return plugins, nil
}
