package v1snap

import (
	"fmt"

	"github.com/spf13/viper"

	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
	pkgconfig "gocache/pkg/config"
)

// init registers v1snap as the embedded snapshot plugin per ADR-0007.
// cmd/server/main.go blank-imports this package to wire the v1 backend
// into the server; resolution happens via
// api/persistence.SnapshotProviderRegistered.
//
// A second persistence plugin compiled into the same binary will panic
// at registration time — see api/persistence.RegisterSnapshotProvider.
func init() {
	apipersistence.RegisterSnapshotProvider(&provider{})
}

// configKey is the YAML / viper key prefix where this plugin reads its
// own configuration. The full key (e.g. plugins.config.snapshot-v1.file)
// is the convention for embedded persistence plugins; each plugin
// claims a sub-key under plugins.config.<plugin-name>.
const configKey = "plugins.config.snapshot-v1"

// keyFile is the plugin's filename setting under configKey.
const keyFile = "file"

// defaultFile is the plugin's default on-disk path. Operators override
// via plugins.config.snapshot-v1.file in YAML, the equivalent
// GOCACHE_PLUGINS_CONFIG_SNAPSHOT-V1_FILE env var, or directly through
// viper.Set in tests.
const defaultFile = "snapshot.dat"

// provider is the bridge between v1snap's concrete Source / Snapshotter
// types and the api/persistence.SnapshotProvider interface. It owns the
// running Source and Snapshotter so the hot-reload callback can update
// their filename on the fly without rebuilding either.
type provider struct {
	src  *Source
	snap *Snapshotter
}

// Name implements api/persistence.SnapshotProvider. Stable identifier
// used for boot-time logs.
func (provider) Name() string { return "v1-snapshot" }

// Build reads the plugin's configuration from the server's viper,
// constructs Source + Snapshotter pointing at the configured file,
// and registers a hot-reload callback so config changes propagate
// to the running halves without a server restart.
//
// Returns an error if viper isn't available — Build is meant to be
// called after pkg/config.Load runs (which wires the global handle);
// any earlier call is a programming bug, not a config problem.
func (p *provider) Build() (apipersistence.Source, apipersistence.Snapshotter, error) {
	v := pkgconfig.Viper()
	if v == nil {
		return nil, nil, fmt.Errorf("v1snap: server viper not initialised — Build called before config.Load")
	}

	v.SetDefault(configKey+"."+keyFile, defaultFile)

	file := v.GetString(configKey + "." + keyFile)
	p.src = NewSource(file)
	p.snap = NewSnapshotter(file)

	pkgconfig.OnReload(p.onReload)

	logger.InfoNoCtx().Str("plugin", "v1-snapshot").Str("file", file).Msg("v1snap: configured")
	return p.src, p.snap, nil
}

// onReload re-reads the plugin's configuration on a server-wide reload
// signal. Today the only knob is the filename — when it changes both
// the Source and Snapshotter SetFilename methods are invoked under
// their internal mutexes, so an in-flight save won't tear.
func (p *provider) onReload(v *viper.Viper) {
	if p.src == nil || p.snap == nil {
		return
	}
	newFile := v.GetString(configKey + "." + keyFile)
	if newFile == "" {
		return
	}
	p.src.SetFilename(newFile)
	p.snap.SetFilename(newFile)
	logger.InfoNoCtx().Str("plugin", "v1-snapshot").Str("file", newFile).Msg("v1snap: filename updated via hot reload")
}
