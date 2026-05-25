package pluginsdk

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RemoteConfig implements api/config.PluginConfig for IPC plugins. It is
// backed by the config map delivered in RegisterAckV1 and updated via
// PluginConfigV1 pushes on hot reload.
//
// Priority (highest wins): os.Getenv via BindEnv > server values > SetDefault.
type RemoteConfig struct {
	mu       sync.RWMutex
	server   map[string]string // from RegisterAckV1.Config / PluginConfigV1
	defaults map[string]any
	envBinds map[string][]string // key → env var names
}

// NewRemoteConfig creates a RemoteConfig seeded with the server-delivered map.
func NewRemoteConfig(server map[string]string) *RemoteConfig {
	if server == nil {
		server = make(map[string]string)
	}
	return &RemoteConfig{
		server:   server,
		defaults: make(map[string]any),
		envBinds: make(map[string][]string),
	}
}

// Replace swaps the server-side config map atomically on hot reload.
func (c *RemoteConfig) Replace(entries map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entries == nil {
		entries = make(map[string]string)
	}
	c.server = entries
}

func (c *RemoteConfig) resolve(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if envVars, ok := c.envBinds[key]; ok {
		for _, ev := range envVars {
			if v := os.Getenv(ev); v != "" {
				return v, true
			}
		}
	}
	if v, ok := c.server[key]; ok {
		return v, true
	}
	if v, ok := c.defaults[key]; ok {
		return stringify(v), true
	}
	return "", false
}

func (c *RemoteConfig) GetString(key string) string {
	v, _ := c.resolve(key)
	return v
}

func (c *RemoteConfig) GetInt(key string) int {
	v, ok := c.resolve(key)
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func (c *RemoteConfig) GetInt64(key string) int64 {
	v, ok := c.resolve(key)
	if !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (c *RemoteConfig) GetBool(key string) bool {
	v, ok := c.resolve(key)
	if !ok {
		return false
	}
	b, _ := strconv.ParseBool(v)
	return b
}

func (c *RemoteConfig) GetDuration(key string) time.Duration {
	v, ok := c.resolve(key)
	if !ok {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Duration(ms) * time.Millisecond
	}
	return 0
}

func (c *RemoteConfig) GetStringSlice(key string) []string {
	v, ok := c.resolve(key)
	if !ok || v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (c *RemoteConfig) IsSet(key string) bool {
	_, ok := c.resolve(key)
	return ok
}

func (c *RemoteConfig) SetDefault(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaults[key] = value
}

func (c *RemoteConfig) BindEnv(key string, envVars ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envBinds[key] = envVars
}

func (c *RemoteConfig) MergeFile(string) error { return nil }

func stringify(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case time.Duration:
		return val.String()
	default:
		return ""
	}
}
