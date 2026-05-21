package persistence

import (
	"fmt"
	"sync"

	apiconfig "gocache/api/config"
)

// SnapshotProvider is the registration handle for an embedded snapshot
// plugin (ADR-0008). Each plugin's init() calls RegisterSnapshotProvider
// with a value of this type; cmd/server/main.go resolves the registered
// provider via SnapshotProviderRegistered.
//
// Selection is done at compile time via blank imports of the desired
// plugin package — there is no config string. Adding a new snapshot
// backend is a new package + a new blank import, with no change to the
// core surface.
//
// The provider is responsible for its own configuration. The server
// hands a typed apiconfig.PluginConfig view to Build; the plugin reads
// scoped keys ("file", "flush") and never names the underlying
// configuration library. Plugins that want hot reload implement
// apiconfig.ReloadHandler — the server type-asserts after Build
// returns and registers the handler on the plugin's behalf.
//
// AOF and replication will get parallel registration interfaces
// (RegisterAOFProvider, RegisterReplicationSink) when those plugins
// land — same pattern, different capability.
type SnapshotProvider interface {
	// Name identifies the provider for logs and diagnostics. Should
	// be a stable plugin identifier (e.g. "snapshot"), not a format
	// string.
	Name() string

	// Build constructs the Source / Snapshotter pair for this provider.
	// The plugin reads its own configuration via cfg — keys are scoped
	// to the plugin's own subsection so the plugin reads "file" rather
	// than the full path. Build is called exactly once per server
	// lifetime, after config load, before any cache traffic.
	//
	// Returns a non-nil error if the plugin cannot configure itself
	// (e.g., required keys missing, file unwritable). The server
	// surfaces the error and may continue without persistence
	// depending on policy.
	Build(cfg apiconfig.PluginConfig) (Source, Snapshotter, error)
}

var (
	snapshotProviderMu sync.RWMutex
	snapshotProvider   SnapshotProvider
)

// RegisterSnapshotProvider installs the embedded snapshot plugin.
// Called from the plugin's init(). Exactly one provider may register
// per binary — a second call panics with the conflicting plugin
// names so the build misconfiguration surfaces at startup, not
// silently as one plugin overwriting another.
//
// The build-time choice of which plugin to import determines which
// provider runs. Multi-tenant binaries (two snapshot plugins coexisting)
// would need a different registration model; this design assumes a
// single canonical provider per binary, matching the "exactly one
// snapshot strategy" reality of every production deployment.
func RegisterSnapshotProvider(p SnapshotProvider) {
	if p == nil {
		panic("api/persistence: RegisterSnapshotProvider called with nil")
	}
	snapshotProviderMu.Lock()
	defer snapshotProviderMu.Unlock()
	if snapshotProvider != nil {
		panic(fmt.Sprintf(
			"api/persistence: snapshot provider already registered (%q); cannot register %q — exactly one provider per binary",
			snapshotProvider.Name(), p.Name(),
		))
	}
	snapshotProvider = p
}

// SnapshotProviderRegistered returns the registered provider, or nil if
// no plugin was imported. Callers (cmd/server/main.go) treat nil as
// "no snapshot persistence" and run the cache in ephemeral mode.
//
// The lookup is mutex-protected because tests may reset the registry
// (see ResetSnapshotProviderForTest) — production-time access happens
// once at startup, well after every plugin's init() has completed.
func SnapshotProviderRegistered() SnapshotProvider {
	snapshotProviderMu.RLock()
	defer snapshotProviderMu.RUnlock()
	return snapshotProvider
}

// ResetSnapshotProviderForTest clears the registered provider. Test-only
// helper exposed so per-package tests that exercise registration can
// run independently. Production code paths must not call this — the
// global is intended to be set exactly once per process lifetime.
func ResetSnapshotProviderForTest() {
	snapshotProviderMu.Lock()
	defer snapshotProviderMu.Unlock()
	snapshotProvider = nil
}

// AOFProvider is the registration handle for an embedded AOF plugin.
// Same pattern as SnapshotProvider — init() registers, cmd/server
// resolves at startup. AOF returns (Source, Sink) because the same
// file serves both roles: Source for boot-time replay, Sink for
// runtime mutation logging.
type AOFProvider interface {
	Name() string
	Build(cfg apiconfig.PluginConfig) (Source, Sink, error)
}

var (
	aofProviderMu sync.RWMutex
	aofProvider   AOFProvider
)

// RegisterAOFProvider installs the embedded AOF plugin.
// Panics on nil or double-register, same as RegisterSnapshotProvider.
func RegisterAOFProvider(p AOFProvider) {
	if p == nil {
		panic("api/persistence: RegisterAOFProvider called with nil")
	}
	aofProviderMu.Lock()
	defer aofProviderMu.Unlock()
	if aofProvider != nil {
		panic(fmt.Sprintf(
			"api/persistence: AOF provider already registered (%q); cannot register %q — exactly one provider per binary",
			aofProvider.Name(), p.Name(),
		))
	}
	aofProvider = p
}

// AOFProviderRegistered returns the registered AOF provider, or nil.
func AOFProviderRegistered() AOFProvider {
	aofProviderMu.RLock()
	defer aofProviderMu.RUnlock()
	return aofProvider
}

// ResetAOFProviderForTest clears the registered AOF provider. Test-only.
func ResetAOFProviderForTest() {
	aofProviderMu.Lock()
	defer aofProviderMu.Unlock()
	aofProvider = nil
}
