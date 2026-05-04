// Package config wires the runtime configuration loader (viper, pflag,
// env vars) onto the data types defined in api/config. The data
// definitions live in api/config so plugins can read them without
// depending on internal server packages; this package owns only the
// loading pipeline.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	apiconfig "gocache/api/config"

	"github.com/fsnotify/fsnotify"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Re-exports of the api/config data types. Internal callers continue to
// use the unqualified config.X names via these aliases.
type (
	Config            = apiconfig.Config
	ServerConfig      = apiconfig.ServerConfig
	PersistenceConfig = apiconfig.PersistenceConfig
	MemoryConfig      = apiconfig.MemoryConfig
	WorkersConfig     = apiconfig.WorkersConfig
	EventsConfig      = apiconfig.EventsConfig
)

// DefaultConfig returns a configuration with sensible defaults.
// Forwarder to api/config.DefaultConfig — kept for back-compat with any
// internal caller that imports pkg/config.
func DefaultConfig() *Config { return apiconfig.DefaultConfig() }

// Default values used by viper.SetDefault below. Mirrored from
// api/config.Default* so the loader and the struct-literal constructor
// agree on numeric defaults. Plugin-specific defaults (not exposed via
// api) are kept here.
const (
	defaultPluginsEnabled     = false
	defaultPluginsDir         = "plugins"
	defaultPluginsSocketPath  = "/tmp/gocache-plugins.sock"
	defaultHealthInterval     = 10 * time.Second
	defaultShutdownTimeout    = 5 * time.Second
	defaultMaxRestarts        = 3
	defaultConnectTimeout     = 10 * time.Second
	defaultMinRestartInterval = 30 * time.Second

	envPrefix         = "GOCACHE"
	defaultConfigName = "gocache"
)

// bindFlag wraps viper.BindPFlag. Callers that register a subset of flags
// (e.g., integration tests with only `--config`) are tolerated: a missing
// flag is a no-op. A BindPFlag error on a non-nil flag is a programmer bug
// and panics so the typo is surfaced at startup instead of being silently
// swallowed by `_ = v.BindPFlag(...)`.
func bindFlag(v *viper.Viper, key string, flags *pflag.FlagSet, flagName string) {
	f := flags.Lookup(flagName)
	if f == nil {
		return
	}
	if err := v.BindPFlag(key, f); err != nil {
		panic(fmt.Sprintf("config: BindPFlag(%s → %s): %v", key, flagName, err))
	}
}

// Load builds a Viper instance from flags, env vars, and config file, then returns the parsed Config.
// Priority: CLI flags > env vars (GOCACHE_*) > config file > defaults
func Load(flags *pflag.FlagSet) (*Config, *viper.Viper, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("server.address", apiconfig.DefaultAddress)
	v.SetDefault("server.port", apiconfig.DefaultPort)
	v.SetDefault("server.log_level", apiconfig.DefaultLogLevel)
	v.SetDefault("server.require_pass", "")
	v.SetDefault("persistence.snapshot_interval", apiconfig.DefaultSnapshotInterval)
	v.SetDefault("persistence.load_on_startup", apiconfig.DefaultLoadOnStartup)
	v.SetDefault("memory.max_memory_mb", apiconfig.DefaultMaxMemoryMB)
	v.SetDefault("memory.eviction_policy", apiconfig.DefaultEvictionPolicy)
	v.SetDefault("memory.cache_shards", apiconfig.DefaultCacheShards)
	v.SetDefault("memory.hash_max_packed_entries", apiconfig.DefaultHashMaxPackedEntries)
	v.SetDefault("memory.hash_max_packed_value", apiconfig.DefaultHashMaxPackedValue)
	v.SetDefault("memory.set_max_packed_entries", apiconfig.DefaultSetMaxPackedEntries)
	v.SetDefault("memory.set_max_packed_value", apiconfig.DefaultSetMaxPackedValue)
	v.SetDefault("memory.zset_max_packed_entries", apiconfig.DefaultZSetMaxPackedEntries)
	v.SetDefault("memory.zset_max_packed_value", apiconfig.DefaultZSetMaxPackedValue)
	v.SetDefault("memory.list_max_packed_size", apiconfig.DefaultListMaxPackedSize)
	v.SetDefault("workers.cleanup_interval", apiconfig.DefaultCleanupInterval)
	v.SetDefault("events.replay_capacity", apiconfig.DefaultEventsReplayCapacity)

	// Plugin defaults
	v.SetDefault("plugins.enabled", defaultPluginsEnabled)
	v.SetDefault("plugins.dir", defaultPluginsDir)
	v.SetDefault("plugins.socket_path", defaultPluginsSocketPath)
	v.SetDefault("plugins.health_interval", defaultHealthInterval)
	v.SetDefault("plugins.shutdown_timeout", defaultShutdownTimeout)
	v.SetDefault("plugins.max_restarts", defaultMaxRestarts)
	v.SetDefault("plugins.connect_timeout", defaultConnectTimeout)
	v.SetDefault("plugins.min_restart_interval_for_replay", defaultMinRestartInterval)

	// Config file — auto-detect format by extension (.yaml/.yml or .json)
	if cfgFile, err := flags.GetString("config"); err == nil && cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName(defaultConfigName)
		v.AddConfigPath(".")
	}
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, nil, fmt.Errorf("failed to read config file: %w", err)
		}
	}

	// Env vars: GOCACHE_SERVER_ADDRESS, GOCACHE_SERVER_PORT, etc.
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Bind CLI flags (only active when the flag is explicitly set)
	bindFlag(v, "server.address", flags, "address")
	bindFlag(v, "server.port", flags, "port")
	bindFlag(v, "server.log_level", flags, "log-level")
	bindFlag(v, "persistence.snapshot_interval", flags, "snapshot-interval")
	bindFlag(v, "persistence.load_on_startup", flags, "load-on-startup")
	bindFlag(v, "memory.max_memory_mb", flags, "max-memory-mb")
	bindFlag(v, "memory.eviction_policy", flags, "eviction-policy")
	bindFlag(v, "workers.cleanup_interval", flags, "cleanup-interval")

	cfg, err := Unmarshal(v)
	if err != nil {
		return nil, v, err
	}

	// Wire viper to the global handle and the reload multiplexer so
	// embedded plugins (and other callers) can subscribe to config
	// reloads via OnReload. Viper's native OnConfigChange is
	// single-callback; we install one fan-out handler here and let
	// callers register independent callbacks against it.
	setServerViper(v)
	v.OnConfigChange(func(_ fsnotify.Event) {
		fireReload(v)
	})
	v.WatchConfig()

	return cfg, v, nil
}

// Reload re-reads the current Viper state into a fresh Config struct.
// Used by the hot-reload callback after a config file change.
func Reload(v *viper.Viper) (*Config, error) {
	return Unmarshal(v)
}

// Unmarshal decodes the Viper state into a Config struct.
func Unmarshal(v *viper.Viper) (*Config, error) {
	var cfg Config
	err := v.Unmarshal(&cfg, viper.DecodeHook(
		mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	))
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	return &cfg, nil
}
