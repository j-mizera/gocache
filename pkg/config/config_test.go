package config_test

import (
	"gocache/pkg/config"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

// newFlagSet creates a fresh flag set with all known flags registered.
// Pass "--flag=value" style args to simulate CLI input.
func newFlagSet(args ...string) *pflag.FlagSet {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("config", "", "path to config file")
	fs.String("address", "", "server listen address")
	fs.Int("port", 0, "server listen port")
	fs.String("snapshot-file", "", "snapshot file path")
	fs.Duration("snapshot-interval", 0, "snapshot interval")
	fs.Bool("load-on-startup", true, "load snapshot on startup")
	fs.Int64("max-memory-mb", 0, "max memory in MB")
	fs.String("eviction-policy", "", "eviction policy")
	fs.Duration("cleanup-interval", 0, "cleanup worker interval")
	_ = fs.Parse(args)
	return fs
}

func TestLoad_Defaults(t *testing.T) {
	fs := newFlagSet()
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.Address != "0.0.0.0" {
		t.Errorf("expected address 0.0.0.0, got %s", cfg.Server.Address)
	}
	if cfg.Server.Port != 6379 {
		t.Errorf("expected port 6379, got %d", cfg.Server.Port)
	}
	if cfg.Persistence.SnapshotFile != "snapshot.dat" {
		t.Errorf("expected snapshot.dat, got %s", cfg.Persistence.SnapshotFile)
	}
	if cfg.Persistence.SnapshotInterval != 5*time.Minute {
		t.Errorf("expected 5m, got %v", cfg.Persistence.SnapshotInterval)
	}
	if !cfg.Persistence.LoadOnStartup {
		t.Errorf("expected load_on_startup true by default")
	}
	if cfg.Memory.MaxMemoryMB != 1024 {
		t.Errorf("expected 1024 MB, got %d", cfg.Memory.MaxMemoryMB)
	}
	if cfg.Memory.EvictionPolicy != "lru" {
		t.Errorf("expected lru, got %s", cfg.Memory.EvictionPolicy)
	}
	if cfg.Workers.CleanupInterval != 1*time.Minute {
		t.Errorf("expected 1m, got %v", cfg.Workers.CleanupInterval)
	}
}

func TestLoad_YAML(t *testing.T) {
	fs := newFlagSet("--config=testdata/config.yaml")
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.Address != "127.0.0.1" {
		t.Errorf("expected 127.0.0.1, got %s", cfg.Server.Address)
	}
	if cfg.Server.Port != 7379 {
		t.Errorf("expected 7379, got %d", cfg.Server.Port)
	}
	if cfg.Persistence.SnapshotFile != "yaml_test.dat" {
		t.Errorf("expected yaml_test.dat, got %s", cfg.Persistence.SnapshotFile)
	}
	if cfg.Persistence.SnapshotInterval != 10*time.Minute {
		t.Errorf("expected 10m, got %v", cfg.Persistence.SnapshotInterval)
	}
	if cfg.Persistence.LoadOnStartup {
		t.Errorf("expected load_on_startup false")
	}
	if cfg.Memory.MaxMemoryMB != 512 {
		t.Errorf("expected 512 MB, got %d", cfg.Memory.MaxMemoryMB)
	}
	if cfg.Memory.EvictionPolicy != "random" {
		t.Errorf("expected random, got %s", cfg.Memory.EvictionPolicy)
	}
	if cfg.Workers.CleanupInterval != 30*time.Second {
		t.Errorf("expected 30s, got %v", cfg.Workers.CleanupInterval)
	}
}

func TestLoad_JSON(t *testing.T) {
	fs := newFlagSet("--config=testdata/config.json")
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.Address != "127.0.0.1" {
		t.Errorf("expected 127.0.0.1, got %s", cfg.Server.Address)
	}
	if cfg.Server.Port != 7379 {
		t.Errorf("expected 7379, got %d", cfg.Server.Port)
	}
	if cfg.Persistence.SnapshotFile != "json_test.dat" {
		t.Errorf("expected json_test.dat, got %s", cfg.Persistence.SnapshotFile)
	}
	if cfg.Persistence.SnapshotInterval != 10*time.Minute {
		t.Errorf("expected 10m, got %v", cfg.Persistence.SnapshotInterval)
	}
	if cfg.Persistence.LoadOnStartup {
		t.Errorf("expected load_on_startup false")
	}
	if cfg.Memory.MaxMemoryMB != 512 {
		t.Errorf("expected 512 MB, got %d", cfg.Memory.MaxMemoryMB)
	}
	if cfg.Memory.EvictionPolicy != "random" {
		t.Errorf("expected random, got %s", cfg.Memory.EvictionPolicy)
	}
	if cfg.Workers.CleanupInterval != 30*time.Second {
		t.Errorf("expected 30s, got %v", cfg.Workers.CleanupInterval)
	}
}

func TestLoad_ExplicitMissingFile(t *testing.T) {
	// When --config points to a file that doesn't exist, Load should return an error.
	// Silent fallback only applies when no --config flag is given (auto-discovery).
	fs := newFlagSet("--config=testdata/nonexistent.yaml")
	_, _, err := config.Load(fs)
	if err == nil {
		t.Error("expected error when explicitly specified config file is missing, got nil")
	}
}

func TestLoad_NoFileUsesDefaults(t *testing.T) {
	// When no --config flag is given and no gocache.yaml exists in the working
	// directory, Load should silently fall back to defaults.
	fs := newFlagSet() // no --config flag; pkg/config/ has no gocache.yaml
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("expected no error when config file is absent, got: %v", err)
	}
	if cfg.Server.Port != 6379 {
		t.Errorf("expected default port 6379, got %d", cfg.Server.Port)
	}
}

func TestLoad_InvalidFile(t *testing.T) {
	fs := newFlagSet("--config=testdata/invalid.yaml")
	_, _, err := config.Load(fs)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestLoad_EnvVars(t *testing.T) {
	t.Setenv("GOCACHE_SERVER_PORT", "9000")
	t.Setenv("GOCACHE_SERVER_ADDRESS", "192.168.1.1")
	t.Setenv("GOCACHE_WORKERS_CLEANUP_INTERVAL", "2m")

	fs := newFlagSet()
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.Port != 9000 {
		t.Errorf("expected port 9000 from env, got %d", cfg.Server.Port)
	}
	if cfg.Server.Address != "192.168.1.1" {
		t.Errorf("expected address 192.168.1.1 from env, got %s", cfg.Server.Address)
	}
	if cfg.Workers.CleanupInterval != 2*time.Minute {
		t.Errorf("expected 2m from env, got %v", cfg.Workers.CleanupInterval)
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	t.Setenv("GOCACHE_SERVER_PORT", "9999")

	fs := newFlagSet("--config=testdata/config.yaml")
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Env var (9999) should beat config file (7379)
	if cfg.Server.Port != 9999 {
		t.Errorf("expected env var port 9999 to override file port 7379, got %d", cfg.Server.Port)
	}
	// Other fields from the file should still be applied
	if cfg.Server.Address != "127.0.0.1" {
		t.Errorf("expected address from file 127.0.0.1, got %s", cfg.Server.Address)
	}
}

func TestLoad_FlagOverridesFile(t *testing.T) {
	fs := newFlagSet("--config=testdata/config.yaml", "--port=8080")
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Flag (8080) should beat config file (7379)
	if cfg.Server.Port != 8080 {
		t.Errorf("expected flag port 8080 to override file port 7379, got %d", cfg.Server.Port)
	}
	// Other fields from the file should still be applied
	if cfg.Server.Address != "127.0.0.1" {
		t.Errorf("expected address from file 127.0.0.1, got %s", cfg.Server.Address)
	}
}

func TestLoad_FlagOverridesEnv(t *testing.T) {
	t.Setenv("GOCACHE_SERVER_PORT", "9000")

	fs := newFlagSet("--port=8080")
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Flag (8080) should beat env var (9000)
	if cfg.Server.Port != 8080 {
		t.Errorf("expected flag port 8080 to override env port 9000, got %d", cfg.Server.Port)
	}
}

func TestLoad_PriorityChain(t *testing.T) {
	// Full priority: flag > env > file > default
	// File sets port=7379, env sets port=9000, flag sets port=8080 → expect 8080
	t.Setenv("GOCACHE_SERVER_PORT", "9000")

	fs := newFlagSet("--config=testdata/config.yaml", "--port=8080")
	cfg, _, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("expected flag (8080) to win priority chain, got %d", cfg.Server.Port)
	}
}

func TestLoad_GetAddr(t *testing.T) {
	tests := []struct {
		name    string
		address string
		port    int
		want    string
	}{
		{"explicit", "127.0.0.1", 7379, "127.0.0.1:7379"},
		{"default port fallback", "0.0.0.0", 0, "0.0.0.0:6379"},
		{"default address fallback", "", 6379, "0.0.0.0:6379"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.ServerConfig{Address: tc.address, Port: tc.port}
			if got := cfg.GetAddr(); got != tc.want {
				t.Errorf("GetAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReload(t *testing.T) {
	fs := newFlagSet("--config=testdata/config.yaml")
	_, v, err := config.Load(fs)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	cfg, err := config.Reload(v)
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	if cfg.Server.Port != 7379 {
		t.Errorf("expected 7379 after reload, got %d", cfg.Server.Port)
	}
	if cfg.Persistence.SnapshotInterval != 10*time.Minute {
		t.Errorf("expected 10m after reload, got %v", cfg.Persistence.SnapshotInterval)
	}
}
