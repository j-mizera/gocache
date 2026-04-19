package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gocache/pkg/plugin"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Default values shared between DefaultConfig and viper.SetDefault. Keeping
// them as named constants guarantees the struct-literal defaults and the
// viper-registered defaults never drift out of sync.
const (
	defaultAddress           = "0.0.0.0"
	defaultPort              = 6379
	defaultLogLevel          = "info"
	defaultSnapshotFile      = "snapshot.dat"
	defaultSnapshotInterval  = 5 * time.Minute
	defaultLoadOnStartup     = true
	defaultMaxMemoryMB       = int64(1024)
	defaultEvictionPolicy    = "lru"
	defaultCleanupInterval   = time.Minute
	defaultEventsReplayCapacity = 10_000
	defaultPluginsEnabled    = false
	defaultPluginsDir        = "plugins"
	defaultPluginsSocketPath = "/tmp/gocache-plugins.sock"
	defaultHealthInterval    = 10 * time.Second
	defaultShutdownTimeout   = 5 * time.Second
	defaultMaxRestarts       = 3
	defaultConnectTimeout    = 10 * time.Second
	defaultMinRestartInterval = 30 * time.Second

	envPrefix         = "GOCACHE"
	defaultConfigName = "gocache"
)

// Config holds all configuration for the GoCache server
type Config struct {
	Server      ServerConfig         `yaml:"server"      mapstructure:"server"`
	Persistence PersistenceConfig    `yaml:"persistence" mapstructure:"persistence"`
	Memory      MemoryConfig         `yaml:"memory"      mapstructure:"memory"`
	Workers     WorkersConfig        `yaml:"workers"     mapstructure:"workers"`
	Events      EventsConfig         `yaml:"events"      mapstructure:"events"`
	Plugins     plugin.PluginsConfig `yaml:"plugins"     mapstructure:"plugins"`
}

// ServerConfig holds server-specific configuration
type ServerConfig struct {
	Address     string `yaml:"address"      mapstructure:"address"`
	Port        int    `yaml:"port"         mapstructure:"port"`
	LogLevel    string `yaml:"log_level"    mapstructure:"log_level"`
	RequirePass string `yaml:"require_pass" mapstructure:"require_pass"`
}

// PersistenceConfig holds persistence configuration
type PersistenceConfig struct {
	SnapshotFile     string        `yaml:"snapshot_file"     mapstructure:"snapshot_file"`
	SnapshotInterval time.Duration `yaml:"snapshot_interval" mapstructure:"snapshot_interval"`
	LoadOnStartup    bool          `yaml:"load_on_startup"   mapstructure:"load_on_startup"`
}

// MemoryConfig holds memory management configuration
type MemoryConfig struct {
	MaxMemoryMB    int64  `yaml:"max_memory_mb"    mapstructure:"max_memory_mb"`
	EvictionPolicy string `yaml:"eviction_policy"  mapstructure:"eviction_policy"`
}

// WorkersConfig holds background worker configuration
type WorkersConfig struct {
	CleanupInterval time.Duration `yaml:"cleanup_interval" mapstructure:"cleanup_interval"`
}

// EventsConfig holds event bus configuration.
//
// ReplayCapacity bounds the ring of retained events used to catch up
// subscribers that connect after boot. 0 disables replay; the bus then
// only forwards live events, mirroring pre-ring behaviour.
type EventsConfig struct {
	ReplayCapacity int `yaml:"replay_capacity" mapstructure:"replay_capacity"`
}

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Address:  defaultAddress,
			Port:     defaultPort,
			LogLevel: defaultLogLevel,
		},
		Persistence: PersistenceConfig{
			SnapshotFile:     defaultSnapshotFile,
			SnapshotInterval: defaultSnapshotInterval,
			LoadOnStartup:    defaultLoadOnStartup,
		},
		Memory: MemoryConfig{
			MaxMemoryMB:    defaultMaxMemoryMB,
			EvictionPolicy: defaultEvictionPolicy,
		},
		Workers: WorkersConfig{
			CleanupInterval: defaultCleanupInterval,
		},
		Events: EventsConfig{
			ReplayCapacity: defaultEventsReplayCapacity,
		},
	}
}

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
	v.SetDefault("server.address", defaultAddress)
	v.SetDefault("server.port", defaultPort)
	v.SetDefault("server.log_level", defaultLogLevel)
	v.SetDefault("server.require_pass", "")
	v.SetDefault("persistence.snapshot_file", defaultSnapshotFile)
	v.SetDefault("persistence.snapshot_interval", defaultSnapshotInterval)
	v.SetDefault("persistence.load_on_startup", defaultLoadOnStartup)
	v.SetDefault("memory.max_memory_mb", defaultMaxMemoryMB)
	v.SetDefault("memory.eviction_policy", defaultEvictionPolicy)
	v.SetDefault("workers.cleanup_interval", defaultCleanupInterval)
	v.SetDefault("events.replay_capacity", defaultEventsReplayCapacity)

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
	bindFlag(v, "persistence.snapshot_file", flags, "snapshot-file")
	bindFlag(v, "persistence.snapshot_interval", flags, "snapshot-interval")
	bindFlag(v, "persistence.load_on_startup", flags, "load-on-startup")
	bindFlag(v, "memory.max_memory_mb", flags, "max-memory-mb")
	bindFlag(v, "memory.eviction_policy", flags, "eviction-policy")
	bindFlag(v, "workers.cleanup_interval", flags, "cleanup-interval")

	cfg, err := Unmarshal(v)
	return cfg, v, err
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

// GetAddr returns the full address string (host:port). It is a pure formatter
// and does not mutate the receiver; defaults are applied via DefaultConfig/Load.
func (c *ServerConfig) GetAddr() string {
	port := c.Port
	if port == 0 {
		port = defaultPort
	}
	addr := c.Address
	if addr == "" {
		addr = defaultAddress
	}
	return fmt.Sprintf("%s:%d", addr, port)
}
