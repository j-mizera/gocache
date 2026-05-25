package config

import (
	"sync"
	"sync/atomic"

	"github.com/spf13/viper"

	apiconfig "gocache/api/config"
)

// serverConfig is the package-private handle that pkg/config holds onto
// after Load runs. The underlying viper instance is an implementation
// detail — callers outside pkg/config never see it. The handle is read
// by Reload, ConfigFileUsed, PluginConfigFor, and the reload
// multiplexer; production code stores into it exactly once.
var serverConfig atomic.Pointer[viper.Viper]

// installHandle stores v as the package-private handle. Called by Load
// once viper is fully wired (defaults + flags + env + file + watch).
func installHandle(v *viper.Viper) {
	serverConfig.Store(v)
	apiconfig.SetPluginConfigProvider(PluginConfigFor)
}

// PluginConfigFor returns a typed read-only view of the named plugin's
// configuration subsection (plugins.config.<name>.*). The returned
// PluginConfig is never nil — when Load has not yet run, the function
// returns a no-op view that yields zero values for every Get* and
// reports IsSet == false.
//
// Plugins call this once at Build time. The view stays valid for the
// lifetime of the server; underlying values are updated automatically
// when the server reloads its config (operators get fresh reads
// without rebuilding the view).
func PluginConfigFor(name string) apiconfig.PluginConfig {
	v := serverConfig.Load()
	if v == nil {
		return nopConfig{}
	}
	return pluginView{v: v, prefix: pluginKeyPrefix(name)}
}

// ConfigFileUsed returns the path to the configuration file that was
// loaded, or the empty string if Load found no file (defaults +
// env + flags only). Used by main.go for boot-time logging.
func ConfigFileUsed() string {
	v := serverConfig.Load()
	if v == nil {
		return ""
	}
	return v.ConfigFileUsed()
}

// OnReload registers fn to run whenever the server's configuration is
// reloaded (fsnotify-driven, fired by viper's OnConfigChange handler
// installed in Load). fn takes no arguments — callers that need the
// fresh Config call Reload from inside the callback, and plugins that
// want a typed view should subscribe via OnPluginReload instead.
//
// The callback runs on the reload goroutine. Implementations should
// be quick — typical use is "re-parse my section, swap an atomic
// pointer, kick a worker". Hot-path work belongs elsewhere.
//
// Safe to call from init() (registration is just an append) or from
// any later boot stage. Reload events fire from Load onwards;
// callbacks registered before Load simply receive every future
// reload.
func OnReload(fn func()) {
	reloadHooksMu.Lock()
	defer reloadHooksMu.Unlock()
	reloadHooks = append(reloadHooks, fn)
}

// OnPluginReload registers a plugin-typed reload handler. The handler
// receives a fresh PluginConfig view scoped to plugins.config.<name>
// on every server reload. Use this when a plugin wants to react to
// runtime config changes without seeing the whole reload pipeline —
// e.g. snapshot plugins that need to swap their on-disk filename.
//
// Plugins normally don't call this directly; the persistence layer
// (and parallel registries — AOF, replication) type-asserts on
// apiconfig.ReloadHandler after Build returns and registers the
// handler on the plugin's behalf.
func OnPluginReload(name string, fn func(apiconfig.PluginConfig)) {
	prefix := pluginKeyPrefix(name)
	OnReload(func() {
		v := serverConfig.Load()
		if v == nil {
			fn(nopConfig{})
			return
		}
		fn(pluginView{v: v, prefix: prefix})
	})
}

var (
	reloadHooksMu sync.Mutex
	reloadHooks   []func()
)

// fireReload invokes every registered reload callback. Called by the
// OnConfigChange handler installed by Load. Snapshots the callback
// list under the mutex so registrations during fan-out don't race
// the iteration.
func fireReload() {
	reloadHooksMu.Lock()
	fns := make([]func(), len(reloadHooks))
	copy(fns, reloadHooks)
	reloadHooksMu.Unlock()

	for _, fn := range fns {
		fn()
	}
}

// resetReloadHooksForTest clears the registered reload callbacks.
// Test-only helper exposed so per-package tests can isolate their
// registrations. Production code must not call this — the global is
// intended to be append-only across the process lifetime.
func resetReloadHooksForTest() {
	reloadHooksMu.Lock()
	defer reloadHooksMu.Unlock()
	reloadHooks = nil
}

// SetForTest installs v as the package-private handle for the duration
// of a test. Returns the previous value so callers can restore via
// t.Cleanup. Production code paths set the handle exactly once via
// pkg/config.Load.
//
// Tests for plugin code should prefer constructing a fake
// apiconfig.PluginConfig directly and passing it to Build; SetForTest
// is meant for tests that need the global handle wired (e.g.
// pkg/config's own tests, integration tests that exercise reload
// fan-out).
func SetForTest(v *viper.Viper) (prev *viper.Viper) {
	prev = serverConfig.Load()
	serverConfig.Store(v)
	return prev
}

// FireReloadForTest invokes registered reload callbacks. Test-only
// helper for callers that want to verify their reload-registered
// handler runs without standing up a real fsnotify pipeline.
func FireReloadForTest() {
	fireReload()
}
