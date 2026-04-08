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

// Config holds all configuration for the GoCache server
type Config struct {
	Server      ServerConfig         `yaml:"server"      mapstructure:"server"`
	Persistence PersistenceConfig    `yaml:"persistence" mapstructure:"persistence"`
	Memory      MemoryConfig         `yaml:"memory"      mapstructure:"memory"`
	Workers     WorkersConfig        `yaml:"workers"     mapstructure:"workers"`
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

// DefaultConfig returns a configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Address:  "0.0.0.0",
			Port:     6379,
			LogLevel: "info",
		},
		Persistence: PersistenceConfig{
			SnapshotFile:     "snapshot.dat",
			SnapshotInterval: 5 * time.Minute,
			LoadOnStartup:    true,
		},
		Memory: MemoryConfig{
			MaxMemoryMB:    1024,
			EvictionPolicy: "lru",
		},
		Workers: WorkersConfig{
			CleanupInterval: 1 * time.Minute,
		},
	}
}

// Load builds a Viper instance from flags, env vars, and config file, then returns the parsed Config.
// Priority: CLI flags > env vars (GOCACHE_*) > config file > defaults
func Load(flags *pflag.FlagSet) (*Config, *viper.Viper, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("server.address", "0.0.0.0")
	v.SetDefault("server.port", 6379)
	v.SetDefault("server.log_level", "info")
	v.SetDefault("server.require_pass", "")
	v.SetDefault("persistence.snapshot_file", "snapshot.dat")
	v.SetDefault("persistence.snapshot_interval", "5m")
	v.SetDefault("persistence.load_on_startup", true)
	v.SetDefault("memory.max_memory_mb", 1024)
	v.SetDefault("memory.eviction_policy", "lru")
	v.SetDefault("workers.cleanup_interval", "1m")

	// Plugin defaults
	v.SetDefault("plugins.enabled", false)
	v.SetDefault("plugins.dir", "plugins")
	v.SetDefault("plugins.socket_path", "/tmp/gocache-plugins.sock")
	v.SetDefault("plugins.health_interval", "10s")
	v.SetDefault("plugins.shutdown_timeout", "5s")
	v.SetDefault("plugins.max_restarts", 3)
	v.SetDefault("plugins.connect_timeout", "10s")

	// Config file — auto-detect format by extension (.yaml/.yml or .json)
	if cfgFile, err := flags.GetString("config"); err == nil && cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName("gocache")
		v.AddConfigPath(".")
	}
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, nil, fmt.Errorf("failed to read config file: %w", err)
		}
	}

	// Env vars: GOCACHE_SERVER_ADDRESS, GOCACHE_SERVER_PORT, etc.
	v.SetEnvPrefix("GOCACHE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Bind CLI flags (only active when the flag is explicitly set)
	_ = v.BindPFlag("server.address", flags.Lookup("address"))
	_ = v.BindPFlag("server.port", flags.Lookup("port"))
	_ = v.BindPFlag("server.log_level", flags.Lookup("log-level"))
	_ = v.BindPFlag("persistence.snapshot_file", flags.Lookup("snapshot-file"))
	_ = v.BindPFlag("persistence.snapshot_interval", flags.Lookup("snapshot-interval"))
	_ = v.BindPFlag("persistence.load_on_startup", flags.Lookup("load-on-startup"))
	_ = v.BindPFlag("memory.max_memory_mb", flags.Lookup("max-memory-mb"))
	_ = v.BindPFlag("memory.eviction_policy", flags.Lookup("eviction-policy"))
	_ = v.BindPFlag("workers.cleanup_interval", flags.Lookup("cleanup-interval"))

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
		port = 6379
	}
	addr := c.Address
	if addr == "" {
		addr = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", addr, port)
}
