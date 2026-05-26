package snapshot

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	apipersistence "gocache/api/persistence"
	"gocache/commons/plugincfg"
)

// TestProvider_Build_ReadsScopedKey verifies the plugin pulls its
// filename from the scoped "file" key (no full path).
func TestProvider_Build_ReadsScopedKey(t *testing.T) {
	dir := t.TempDir()
	wantFile := filepath.Join(dir, "configured.snap")

	cfg := plugincfg.NewMapConfig()
	cfg.Values[keyFile] = wantFile

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
	cfg := plugincfg.NewMapConfig()

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
	cfg := plugincfg.NewMapConfig()
	cfg.Values[keyFile] = "" // explicit empty wins over the SetDefault inside Build

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

	cfgInitial := plugincfg.NewMapConfig()
	cfgInitial.Values[keyFile] = first

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

	cfgReloaded := plugincfg.NewMapConfig()
	cfgReloaded.Values[keyFile] = second
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
