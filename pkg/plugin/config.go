package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

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
type PluginOverride struct {
	Binary   string   `yaml:"binary"   mapstructure:"binary"`
	Critical bool     `yaml:"critical" mapstructure:"critical"`
	Priority int      `yaml:"priority" mapstructure:"priority"`
	Scopes   []string `yaml:"scopes"   mapstructure:"scopes"`
}

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
		if fi.Mode()&0111 == 0 {
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
			entry.Critical = override.Critical
			entry.Priority = override.Priority
		}

		plugins = append(plugins, entry)
	}

	return plugins, nil
}
