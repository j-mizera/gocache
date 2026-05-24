package snapshot

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiconfig "gocache/api/config"
	apipersistence "gocache/api/persistence"
)

// mapConfig is the test stand-in for apiconfig.PluginConfig. It keeps
// values in a plain map so tests can construct a config view with
// canned values and pass it directly to Build / OnConfigReload — no
// global state, no swap-and-restore on a process-wide handle.
type mapConfig struct {
	values   map[string]any
	defaults map[string]any
}

func newMapConfig() *mapConfig {
	return &mapConfig{values: map[string]any{}, defaults: map[string]any{}}
}

func (m *mapConfig) lookup(key string) any {
	if v, ok := m.values[key]; ok {
		return v
	}
	if v, ok := m.defaults[key]; ok {
		return v
	}
	return nil
}

func (m *mapConfig) GetString(key string) string {
	if v, ok := m.lookup(key).(string); ok {
		return v
	}
	return ""
}
func (m *mapConfig) GetInt(key string) int {
	if v, ok := m.lookup(key).(int); ok {
		return v
	}
	return 0
}
func (m *mapConfig) GetInt64(key string) int64 {
	if v, ok := m.lookup(key).(int64); ok {
		return v
	}
	return 0
}
func (m *mapConfig) GetBool(key string) bool {
	if v, ok := m.lookup(key).(bool); ok {
		return v
	}
	return false
}
func (m *mapConfig) GetDuration(key string) time.Duration {
	if v, ok := m.lookup(key).(time.Duration); ok {
		return v
	}
	return 0
}
func (m *mapConfig) GetStringSlice(key string) []string {
	if v, ok := m.lookup(key).([]string); ok {
		return v
	}
	return nil
}
func (m *mapConfig) IsSet(key string) bool {
	_, ok := m.values[key]
	return ok
}
func (m *mapConfig) SetDefault(key string, value any) {
	m.defaults[key] = value
}

var _ apiconfig.PluginConfig = (*mapConfig)(nil)

// TestProvider_Build_ReadsScopedKey verifies the plugin pulls its
// filename from the scoped "file" key (no full path).
func TestProvider_Build_ReadsScopedKey(t *testing.T) {
	dir := t.TempDir()
	wantFile := filepath.Join(dir, "configured.snap")

	cfg := newMapConfig()
	cfg.values[keyFile] = wantFile

	p := &provider{}
	backend, err := p.Build(cfg, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if backend.Source == nil || backend.Snapshotter == nil {
		t.Fatalf("Build returned nil fields: Source=%v Snapshotter=%v", backend.Source, backend.Snapshotter)
	}

	if err := backend.Snapshotter.SaveSnapshot(context.Background(), emptySrc{}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if _, err := os.Stat(wantFile); err != nil {
		t.Errorf("snapshot not at configured path: %v", err)
	}
}

// TestProvider_Build_AppliesDefault verifies the plugin's SetDefault
// call provides a fallback when the operator hasn't set "file".
func TestProvider_Build_AppliesDefault(t *testing.T) {
	cfg := newMapConfig()

	p := &provider{}
	if _, err := p.Build(cfg, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got := cfg.GetString(keyFile); got != defaultFile {
		t.Errorf("default not applied: got %q, want %q", got, defaultFile)
	}
}

// TestProvider_Build_RequiredKeyEmpty verifies that an empty filename
// (e.g. operator explicitly set the key to "") surfaces as an error.
func TestProvider_Build_RequiredKeyEmpty(t *testing.T) {
	cfg := newMapConfig()
	cfg.values[keyFile] = "" // explicit empty wins over the SetDefault inside Build

	p := &provider{}
	_, err := p.Build(cfg, nil)
	if err == nil {
		t.Error("expected error when file resolves to empty string, got none")
	}
}

// TestProvider_OnConfigReload exercises the reload path: starting from
// one filename, handing the provider a fresh PluginConfig with the
// new value updates the live Source/Snapshotter without restart.
func TestProvider_OnConfigReload(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.snap")
	second := filepath.Join(dir, "second.snap")

	cfgInitial := newMapConfig()
	cfgInitial.values[keyFile] = first

	p := &provider{}
	backend, err := p.Build(cfgInitial, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	snap := backend.Snapshotter

	if err := snap.SaveSnapshot(context.Background(), emptySrc{}); err != nil {
		t.Fatalf("SaveSnapshot first: %v", err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Errorf("first snapshot missing: %v", err)
	}

	cfgReloaded := newMapConfig()
	cfgReloaded.values[keyFile] = second
	p.OnConfigReload(cfgReloaded)

	if err := snap.SaveSnapshot(context.Background(), emptySrc{}); err != nil {
		t.Fatalf("SaveSnapshot second: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Errorf("second snapshot missing after reload: %v", err)
	}
}

// emptySrc is a SnapshotSource yielding zero entries — enough for the
// writer to produce a valid empty snapshot file.
type emptySrc struct{}

func (emptySrc) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	return apipersistence.SnapshotEntry{}, io.EOF
}
