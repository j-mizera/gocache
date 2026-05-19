// Package config holds the runtime configuration types exposed to plugins
// and other api consumers. Loading (viper, pflag, env vars) lives in
// pkg/config — this package contains only pure data definitions and
// constructors so plugins can read configuration without depending on
// internal server packages.
package config

import (
	"fmt"
	"time"

	apiplugin "gocache/api/plugin"
)

// Default values exposed for callers that need to recognize "still at
// default" semantics (e.g. tests, embedded plugins). The viper layer in
// pkg/config registers the same numeric defaults; keeping them here makes
// DefaultConfig a single source of truth for the struct-literal path.
const (
	DefaultAddress          = "0.0.0.0"
	DefaultPort             = 6379
	DefaultLogLevel         = "info"
	DefaultSnapshotInterval = 5 * time.Minute
	DefaultLoadOnStartup    = true
	DefaultMaxMemoryMB      = int64(1024)
	DefaultEvictionPolicy   = "lru"
	DefaultCacheShards      = 256

	// Hybrid-encoding thresholds. Defaults match Valkey 8 (src/config.c).
	// Collections that exceed either threshold (count or per-item length)
	// are promoted from the byte-packed encoding to native Go shapes.
	DefaultHashMaxPackedEntries = 512
	DefaultHashMaxPackedValue   = 64
	DefaultSetMaxPackedEntries  = 128
	DefaultSetMaxPackedValue    = 64
	DefaultZSetMaxPackedEntries = 128
	DefaultZSetMaxPackedValue   = 64
	DefaultListMaxPackedSize    = 8192 // bytes, not entries

	DefaultCleanupInterval      = time.Minute
	DefaultEventsReplayCapacity = 10_000
)

// Config holds all configuration for the GoCache server.
type Config struct {
	Server      ServerConfig            `yaml:"server"      mapstructure:"server"`
	Persistence PersistenceConfig       `yaml:"persistence" mapstructure:"persistence"`
	Memory      MemoryConfig            `yaml:"memory"      mapstructure:"memory"`
	Workers     WorkersConfig           `yaml:"workers"     mapstructure:"workers"`
	Events      EventsConfig            `yaml:"events"      mapstructure:"events"`
	Plugins     apiplugin.PluginsConfig `yaml:"plugins"     mapstructure:"plugins"`
}

// ServerConfig holds server-specific configuration.
type ServerConfig struct {
	Address     string `yaml:"address"      mapstructure:"address"`
	Port        int    `yaml:"port"         mapstructure:"port"`
	LogLevel    string `yaml:"log_level"    mapstructure:"log_level"`
	RequirePass string `yaml:"require_pass" mapstructure:"require_pass"`
}

// PersistenceConfig holds the server-orchestration knobs for the
// persistence subsystem. Plugin-specific keys (filenames, formats,
// per-plugin tunables) live under plugins.config.<plugin-name> in
// the YAML and are read by each plugin through the typed
// apiconfig.PluginConfig view it receives in Build. The server only
// owns the orchestration policy: whether to recover at startup and
// how often to schedule periodic snapshot saves.
//
// See ADR-0008 for the rationale.
type PersistenceConfig struct {
	SnapshotInterval time.Duration `yaml:"snapshot_interval" mapstructure:"snapshot_interval"`
	LoadOnStartup    bool          `yaml:"load_on_startup"   mapstructure:"load_on_startup"`
}

// MemoryConfig holds memory management configuration.
//
// The HashMaxPacked*, SetMaxPacked*, ZSetMaxPacked*, and ListMaxPackedSize
// fields govern hybrid collection encoding. Small collections are stored as
// a flat byte buffer (Packed) and mutated in place; when a collection
// exceeds one of the thresholds it is promoted to a native Go map/slice
// (Native). Defaults mirror Valkey 8.
type MemoryConfig struct {
	MaxMemoryMB    int64  `yaml:"max_memory_mb"    mapstructure:"max_memory_mb"`
	EvictionPolicy string `yaml:"eviction_policy"  mapstructure:"eviction_policy"`

	// CacheShards is the number of cache shards (and engine goroutines).
	// Must be a positive power of two.
	CacheShards int `yaml:"cache_shards" mapstructure:"cache_shards"`

	HashMaxPackedEntries int `yaml:"hash_max_packed_entries" mapstructure:"hash_max_packed_entries"`
	HashMaxPackedValue   int `yaml:"hash_max_packed_value"   mapstructure:"hash_max_packed_value"`
	SetMaxPackedEntries  int `yaml:"set_max_packed_entries"  mapstructure:"set_max_packed_entries"`
	SetMaxPackedValue    int `yaml:"set_max_packed_value"    mapstructure:"set_max_packed_value"`
	ZSetMaxPackedEntries int `yaml:"zset_max_packed_entries" mapstructure:"zset_max_packed_entries"`
	ZSetMaxPackedValue   int `yaml:"zset_max_packed_value"   mapstructure:"zset_max_packed_value"`
	ListMaxPackedSize    int `yaml:"list_max_packed_size"    mapstructure:"list_max_packed_size"`
}

// WorkersConfig holds background worker configuration.
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

// DefaultConfig returns a configuration with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Address:  DefaultAddress,
			Port:     DefaultPort,
			LogLevel: DefaultLogLevel,
		},
		Persistence: PersistenceConfig{
			SnapshotInterval: DefaultSnapshotInterval,
			LoadOnStartup:    DefaultLoadOnStartup,
		},
		Memory: MemoryConfig{
			MaxMemoryMB:          DefaultMaxMemoryMB,
			EvictionPolicy:       DefaultEvictionPolicy,
			CacheShards:          DefaultCacheShards,
			HashMaxPackedEntries: DefaultHashMaxPackedEntries,
			HashMaxPackedValue:   DefaultHashMaxPackedValue,
			SetMaxPackedEntries:  DefaultSetMaxPackedEntries,
			SetMaxPackedValue:    DefaultSetMaxPackedValue,
			ZSetMaxPackedEntries: DefaultZSetMaxPackedEntries,
			ZSetMaxPackedValue:   DefaultZSetMaxPackedValue,
			ListMaxPackedSize:    DefaultListMaxPackedSize,
		},
		Workers: WorkersConfig{
			CleanupInterval: DefaultCleanupInterval,
		},
		Events: EventsConfig{
			ReplayCapacity: DefaultEventsReplayCapacity,
		},
	}
}

// GetAddr returns the full address string (host:port). It is a pure formatter
// and does not mutate the receiver; defaults are applied via DefaultConfig/Load.
func (c *ServerConfig) GetAddr() string {
	port := c.Port
	if port == 0 {
		port = DefaultPort
	}
	addr := c.Address
	if addr == "" {
		addr = DefaultAddress
	}
	return fmt.Sprintf("%s:%d", addr, port)
}
