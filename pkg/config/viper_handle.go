package config

import (
	"sync"
	"sync/atomic"

	"github.com/spf13/viper"
)

// Viper returns the server's viper instance, or nil before Load has run.
// Embedded plugins (per ADR-0007) use it to read their own config
// sub-section and to subscribe to hot reload via OnReload.
//
// Returns nil during plugin init() because Load runs from main()
// after every package's init has fired. Plugins that need viper
// at boot time grab it inside SnapshotProvider.Build, which is
// called by main.go after Load completes.
func Viper() *viper.Viper {
	return serverViper.Load()
}

var serverViper atomic.Pointer[viper.Viper]

// setServerViper installs the global viper handle. Called by Load
// once viper is fully wired (defaults + flags + env + file + watch).
func setServerViper(v *viper.Viper) {
	serverViper.Store(v)
}

// OnReload registers fn to be invoked whenever the server's viper
// reloads its config (fsnotify-driven, fired by viper's own
// OnConfigChange). Multiple plugins can register independent
// callbacks via this multiplexer — viper's native OnConfigChange
// is single-callback-per-instance, so the server hijacks it once
// in Load and fans out to every registered callback here.
//
// The callback runs on viper's reload goroutine. Implementations
// should be quick — typical use is "re-read my subsection, update
// internal state". Hot-path work belongs elsewhere.
//
// Safe to call from init() (registration is just an append) or
// from Build (after Load). Reload events fire from Load onwards;
// callbacks registered before Load will simply receive every
// future reload.
func OnReload(fn func(*viper.Viper)) {
	reloadHooksMu.Lock()
	defer reloadHooksMu.Unlock()
	reloadHooks = append(reloadHooks, fn)
}

var (
	reloadHooksMu sync.Mutex
	reloadHooks   []func(*viper.Viper)
)

// fireReload invokes every registered reload callback with v. Called
// by the OnConfigChange handler installed by Load. Snapshots the
// callback list under the mutex so registrations during fan-out
// don't race the iteration.
func fireReload(v *viper.Viper) {
	reloadHooksMu.Lock()
	fns := make([]func(*viper.Viper), len(reloadHooks))
	copy(fns, reloadHooks)
	reloadHooksMu.Unlock()

	for _, fn := range fns {
		fn(v)
	}
}

// resetReloadHooksForTest clears the registered reload callbacks.
// Test-only helper exposed so per-package tests can isolate their
// registrations. Production code must not call this — the global
// is intended to be append-only across the process lifetime.
func resetReloadHooksForTest() {
	reloadHooksMu.Lock()
	defer reloadHooksMu.Unlock()
	reloadHooks = nil
}

// SetViperForTest installs v as the global server viper for the
// duration of a test. Returns the previous value so callers can
// restore state via t.Cleanup. Only embedded plugin tests should
// reach for this — production code paths set the viper exactly
// once via pkg/config.Load.
func SetViperForTest(v *viper.Viper) (prev *viper.Viper) {
	prev = serverViper.Load()
	serverViper.Store(v)
	return prev
}

// FireReloadForTest invokes registered reload callbacks with v.
// Test-only helper for plugins that want to verify their
// OnReload-registered handler runs without standing up a real
// fsnotify pipeline.
func FireReloadForTest(v *viper.Viper) {
	fireReload(v)
}
