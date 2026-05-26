package plugincfg

import (
	"sync/atomic"

	"gocache/api/config"
)

// pluginConfigProvider is set once by the config loader (pkg/config)
// after the server config is loaded.
var pluginConfigProvider atomic.Value // stores func(string) config.PluginConfig

// SetPluginConfigProvider registers the function that produces scoped
// PluginConfig views. Called once by pkg/config during installHandle.
func SetPluginConfigProvider(fn func(string) config.PluginConfig) {
	pluginConfigProvider.Store(fn)
}

// PluginConfigFor returns a scoped PluginConfig view for the named
// plugin. Safe to call from ConfigLoaded and later. Before the config
// loader runs, returns a zero-value MapConfig (all Gets return zero,
// SetDefault and BindEnv are no-ops).
func PluginConfigFor(name string) config.PluginConfig {
	if fn, ok := pluginConfigProvider.Load().(func(string) config.PluginConfig); ok {
		return fn(name)
	}
	return NewMapConfig()
}
