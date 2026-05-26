package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	apiplugin "gocache/api/plugin"
	"gocache/commons/logger"
)

// Re-exports of the public plugin contract types. Pure data definitions
// live in api/plugin so plugin authors never need to depend on pkg/.
// Internal callers keep using the unqualified plugin.X names via these
// aliases.
type (
	PluginsConfig  = apiplugin.PluginsConfig
	PluginOverride = apiplugin.PluginOverride
)

const (
	FailurePolicyContinue   = apiplugin.FailurePolicyContinue
	FailurePolicyHaltServer = apiplugin.FailurePolicyHaltServer
)

var deprecatedCriticalWarnOnce sync.Once

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

			switch override.FailurePolicy {
			case FailurePolicyHaltServer, FailurePolicyContinue, "":
			default:
				logger.WarnNoCtx().Str("failure_policy", override.FailurePolicy).
					Msg("unknown plugin failure_policy — falling back to legacy 'critical' field")
			}
			if override.Critical && override.FailurePolicy != FailurePolicyHaltServer && override.FailurePolicy != FailurePolicyContinue {
				deprecatedCriticalWarnOnce.Do(func() {
					logger.WarnNoCtx().Msg("plugin override uses deprecated 'critical: true' — migrate to 'failure_policy: halt_server'")
				})
			}
		}

		plugins = append(plugins, entry)
	}

	return plugins, nil
}
