package plugincfg

import (
	"time"

	"gocache/api/config"
)

// MapConfig is a stand-in for PluginConfig that stores values in plain
// maps. Use it in plugin tests to construct a config view with canned
// values — no global state, no viper dependency. Also used as the
// zero-value fallback before the config loader runs.
type MapConfig struct {
	Values   map[string]any
	Defaults map[string]any
}

var _ config.PluginConfig = (*MapConfig)(nil)

// NewMapConfig returns a ready-to-use test config.
func NewMapConfig() *MapConfig {
	return &MapConfig{Values: map[string]any{}, Defaults: map[string]any{}}
}

func (m *MapConfig) lookup(key string) any {
	if v, ok := m.Values[key]; ok {
		return v
	}
	if v, ok := m.Defaults[key]; ok {
		return v
	}
	return nil
}

func (m *MapConfig) GetString(key string) string {
	if v, ok := m.lookup(key).(string); ok {
		return v
	}
	return ""
}

func (m *MapConfig) GetInt(key string) int {
	if v, ok := m.lookup(key).(int); ok {
		return v
	}
	return 0
}

func (m *MapConfig) GetInt64(key string) int64 {
	if v, ok := m.lookup(key).(int64); ok {
		return v
	}
	return 0
}

func (m *MapConfig) GetBool(key string) bool {
	if v, ok := m.lookup(key).(bool); ok {
		return v
	}
	return false
}

func (m *MapConfig) GetDuration(key string) time.Duration {
	if v, ok := m.lookup(key).(time.Duration); ok {
		return v
	}
	return 0
}

func (m *MapConfig) GetStringSlice(key string) []string {
	if v, ok := m.lookup(key).([]string); ok {
		return v
	}
	return nil
}

func (m *MapConfig) IsSet(key string) bool {
	_, ok := m.Values[key]
	return ok
}

func (m *MapConfig) SetDefault(key string, value any) {
	m.Defaults[key] = value
}

func (m *MapConfig) BindEnv(string, ...string) {}
func (m *MapConfig) MergeFile(string) error    { return nil }
