package v1snap

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"

	apipersistence "gocache/api/persistence"
	pkgconfig "gocache/pkg/config"
)

// swapServerViper installs v as the global server viper for the
// duration of the test. Restores whatever was there before on Cleanup,
// so test order doesn't matter.
func swapServerViper(t *testing.T, v *viper.Viper) {
	t.Helper()
	prev := pkgconfig.SetViperForTest(v)
	t.Cleanup(func() { pkgconfig.SetViperForTest(prev) })
}

// TestProvider_Build_ReadsViperSubsection verifies the plugin pulls
// its filename from plugins.config.snapshot-v1.file rather than from
// any server-config struct field.
func TestProvider_Build_ReadsViperSubsection(t *testing.T) {
	dir := t.TempDir()
	wantFile := filepath.Join(dir, "configured.snap")

	v := viper.New()
	v.Set("plugins.config.snapshot-v1.file", wantFile)
	swapServerViper(t, v)

	p := &provider{}
	src, snap, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if src == nil || snap == nil {
		t.Fatalf("Build returned nil: src=%v snap=%v", src, snap)
	}

	// Save a snapshot via the plugin and verify it lands at the
	// configured path — proves the filename plumbing connected.
	if err := snap.SaveSnapshot(context.Background(), emptySrc{}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if _, err := os.Stat(wantFile); err != nil {
		t.Errorf("snapshot not at configured path: %v", err)
	}
}

// TestProvider_Build_AppliesDefault verifies the plugin sets a default
// filename when the viper subsection key is absent.
func TestProvider_Build_AppliesDefault(t *testing.T) {
	v := viper.New()
	swapServerViper(t, v)

	p := &provider{}
	if _, _, err := p.Build(); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got := v.GetString("plugins.config.snapshot-v1.file"); got != defaultFile {
		t.Errorf("default not applied: got %q, want %q", got, defaultFile)
	}
}

// TestProvider_Build_NilViper verifies Build refuses to run before
// pkg/config.Load wires the server viper.
func TestProvider_Build_NilViper(t *testing.T) {
	swapServerViper(t, nil)

	p := &provider{}
	_, _, err := p.Build()
	if err == nil {
		t.Error("expected error when viper is nil, got none")
	}
}

// TestProvider_HotReload exercises the OnReload callback: starting
// from one filename, changing the viper key, and triggering reload
// updates the live Source/Snapshotter.
func TestProvider_HotReload(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.snap")
	second := filepath.Join(dir, "second.snap")

	v := viper.New()
	v.Set("plugins.config.snapshot-v1.file", first)
	swapServerViper(t, v)

	p := &provider{}
	_, snap, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if err := snap.SaveSnapshot(context.Background(), emptySrc{}); err != nil {
		t.Fatalf("SaveSnapshot first: %v", err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Errorf("first snapshot missing: %v", err)
	}

	// Simulate config reload — change the key, fire the callback
	// directly. Production fires it via pkg/config.fireReload after
	// viper's fsnotify watcher detects the file change.
	v.Set("plugins.config.snapshot-v1.file", second)
	p.onReload(v)

	if err := snap.SaveSnapshot(context.Background(), emptySrc{}); err != nil {
		t.Fatalf("SaveSnapshot second: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Errorf("second snapshot missing after hot reload: %v", err)
	}
}

// emptySrc is a SnapshotSource yielding zero entries — enough for
// the writer to produce a valid empty v1 file.
type emptySrc struct{}

func (emptySrc) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	return apipersistence.SnapshotEntry{}, io.EOF
}
