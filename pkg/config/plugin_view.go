package config

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/viper"

	apiconfig "gocache/api/config"
)

// pluginKeyPrefix returns the YAML / viper key prefix where the named
// plugin's configuration lives. Centralised here so a future change of
// convention (e.g. plugins.embedded.<name>) only touches one site.
func pluginKeyPrefix(name string) string {
	return "plugins.config." + name
}

// pluginView is the viper-backed implementation of
// apiconfig.PluginConfig. Reads delegate to the wrapped viper, with
// keys qualified by prefix so plugins read scoped names ("file") that
// resolve to the full key (plugins.config.<name>.file) inside viper.
//
// pluginView is intentionally unexported. Plugins receive it as
// apiconfig.PluginConfig — the concrete type, like the underlying
// configuration library, is an implementation detail of pkg/config.
type pluginView struct {
	v      *viper.Viper
	prefix string
}

func (p pluginView) qualified(key string) string {
	if key == "" {
		return p.prefix
	}
	return p.prefix + "." + key
}

func (p pluginView) GetString(key string) string             { return p.v.GetString(p.qualified(key)) }
func (p pluginView) GetInt(key string) int                   { return p.v.GetInt(p.qualified(key)) }
func (p pluginView) GetInt64(key string) int64               { return p.v.GetInt64(p.qualified(key)) }
func (p pluginView) GetBool(key string) bool                 { return p.v.GetBool(p.qualified(key)) }
func (p pluginView) GetDuration(key string) time.Duration    { return p.v.GetDuration(p.qualified(key)) }
func (p pluginView) GetStringSlice(key string) []string      { return p.v.GetStringSlice(p.qualified(key)) }
func (p pluginView) IsSet(key string) bool                   { return p.v.IsSet(p.qualified(key)) }
func (p pluginView) SetDefault(key string, value any)        { p.v.SetDefault(p.qualified(key), value) }

func (p pluginView) BindEnv(key string, envVars ...string) {
	_ = p.v.BindEnv(append([]string{p.qualified(key)}, envVars...)...)
}

func (p pluginView) MergeFile(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	tmp := viper.New()
	tmp.SetConfigFile(path)
	if err := tmp.ReadInConfig(); err != nil {
		return fmt.Errorf("plugin config merge: %w", err)
	}
	for _, key := range tmp.AllKeys() {
		qualified := p.qualified(key)
		if !p.v.IsSet(qualified) {
			p.v.SetDefault(qualified, tmp.Get(key))
		}
	}
	return nil
}

// Compile-time check that pluginView satisfies the contract.
var _ apiconfig.PluginConfig = pluginView{}

// nopConfig is the zero-value PluginConfig returned when Load has not
// run yet (e.g. unit tests that exercise plugin code paths without
// standing up the full server). Every Get* returns the type's zero
// value; IsSet returns false; SetDefault is a no-op.
//
// nopConfig keeps PluginConfigFor never-nil — plugin code can call
// cfg.GetString("file") without a defensive nil check, and the empty
// return cleanly hits the plugin's required-key error path.
type nopConfig struct{}

func (nopConfig) GetString(string) string             { return "" }
func (nopConfig) GetInt(string) int                   { return 0 }
func (nopConfig) GetInt64(string) int64               { return 0 }
func (nopConfig) GetBool(string) bool                 { return false }
func (nopConfig) GetDuration(string) time.Duration    { return 0 }
func (nopConfig) GetStringSlice(string) []string      { return nil }
func (nopConfig) IsSet(string) bool                   { return false }
func (nopConfig) SetDefault(string, any)              {}
func (nopConfig) BindEnv(string, ...string)           {}
func (nopConfig) MergeFile(string) error              { return nil }

var _ apiconfig.PluginConfig = nopConfig{}
