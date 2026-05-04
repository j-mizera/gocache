package snapshot

import (
	"fmt"

	apiconfig "gocache/api/config"
	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
)

// init registers snapshot as the embedded snapshot plugin per ADR-0008.
// cmd/server/main.go blank-imports this package to wire the binary
// snapshot backend into the server; resolution happens via
// api/persistence.SnapshotProviderRegistered.
//
// A second snapshot plugin compiled into the same binary will panic at
// registration time — see api/persistence.RegisterSnapshotProvider.
func init() {
	apipersistence.RegisterSnapshotProvider(&provider{})
}

// keyFile is the plugin's filename setting. Plugins read scoped keys
// via the typed PluginConfig view, so the fully qualified path
// (plugins.config.snapshot.file) never appears here.
const keyFile = "file"

// defaultFile is the plugin's default on-disk path. provider.Build
// declares the fallback explicitly via cfg.SetDefault — absence of
// SetDefault would make the key required and Build would surface
// the missing value as an error.
const defaultFile = "snapshot.dat"

// provider bridges the concrete Source / Snapshotter types and the
// api/persistence.SnapshotProvider interface. It owns the running
// Source and Snapshotter so the OnConfigReload callback can update
// their filename without rebuilding either.
//
// provider also implements apiconfig.ReloadHandler — the server
// type-asserts on it after Build returns and routes reload events
// here automatically.
type provider struct {
	src  *Source
	snap *Snapshotter
}

// Name implements api/persistence.SnapshotProvider. Stable identifier
// used for boot-time logs and as the YAML key prefix
// (plugins.config.<Name()>).
func (provider) Name() string { return "snapshot" }

// Build reads the plugin's configuration from the typed PluginConfig
// view, constructs Source + Snapshotter pointing at the configured
// file, and returns them. Hot reload is wired by the server: it
// type-asserts the provider on apiconfig.ReloadHandler after Build
// returns and registers OnConfigReload via pkg/config.OnPluginReload.
func (p *provider) Build(cfg apiconfig.PluginConfig) (apipersistence.Source, apipersistence.Snapshotter, error) {
	cfg.SetDefault(keyFile, defaultFile)
	file := cfg.GetString(keyFile)
	if file == "" {
		return nil, nil, fmt.Errorf("snapshot: %q is required", keyFile)
	}

	p.src = NewSource(file)
	p.snap = NewSnapshotter(file)

	logger.InfoNoCtx().Str("plugin", "snapshot").Str("file", file).Msg("snapshot: configured")
	return p.src, p.snap, nil
}

// OnConfigReload implements apiconfig.ReloadHandler. Re-reads the
// plugin's keys on a server-wide reload signal; today the only knob
// is the filename, swapped under the Source/Snapshotter mutex so an
// in-flight save won't tear.
func (p *provider) OnConfigReload(cfg apiconfig.PluginConfig) {
	if p.src == nil || p.snap == nil {
		return
	}
	newFile := cfg.GetString(keyFile)
	if newFile == "" {
		return
	}
	p.src.SetFilename(newFile)
	p.snap.SetFilename(newFile)
	logger.InfoNoCtx().Str("plugin", "snapshot").Str("file", newFile).Msg("snapshot: filename updated via hot reload")
}
