package pluginsdk

import (
	"testing"
	"time"

	apiconfig "gocache/api/config"
)

var _ apiconfig.PluginConfig = (*RemoteConfig)(nil)

func TestRemoteConfig_ServerValues(t *testing.T) {
	rc := NewRemoteConfig(map[string]string{
		"file":  "appendonly.aof",
		"flush": "5s",
		"count": "42",
	})
	if got := rc.GetString("file"); got != "appendonly.aof" {
		t.Errorf("GetString = %q, want %q", got, "appendonly.aof")
	}
	if got := rc.GetInt("count"); got != 42 {
		t.Errorf("GetInt = %d, want %d", got, 42)
	}
	if got := rc.GetDuration("flush"); got != 5*time.Second {
		t.Errorf("GetDuration = %v, want %v", got, 5*time.Second)
	}
	if !rc.IsSet("file") {
		t.Error("IsSet should return true for server key")
	}
	if rc.IsSet("missing") {
		t.Error("IsSet should return false for missing key")
	}
}

func TestRemoteConfig_SetDefault(t *testing.T) {
	rc := NewRemoteConfig(nil)
	rc.SetDefault("file", "default.aof")
	if got := rc.GetString("file"); got != "default.aof" {
		t.Errorf("SetDefault: got %q, want %q", got, "default.aof")
	}
}

func TestRemoteConfig_ServerOverridesDefault(t *testing.T) {
	rc := NewRemoteConfig(map[string]string{"file": "server.aof"})
	rc.SetDefault("file", "default.aof")
	if got := rc.GetString("file"); got != "server.aof" {
		t.Errorf("server should override default: got %q", got)
	}
}

func TestRemoteConfig_BindEnvOverridesServer(t *testing.T) {
	t.Setenv("GOCACHE_AOF_FILE", "env.aof")
	rc := NewRemoteConfig(map[string]string{"file": "server.aof"})
	rc.BindEnv("file", "GOCACHE_AOF_FILE")
	if got := rc.GetString("file"); got != "env.aof" {
		t.Errorf("env should override server: got %q", got)
	}
}

func TestRemoteConfig_Replace(t *testing.T) {
	rc := NewRemoteConfig(map[string]string{"file": "old.aof"})
	if got := rc.GetString("file"); got != "old.aof" {
		t.Fatalf("before replace: got %q", got)
	}
	rc.Replace(map[string]string{"file": "new.aof", "extra": "val"})
	if got := rc.GetString("file"); got != "new.aof" {
		t.Errorf("after replace: got %q, want %q", got, "new.aof")
	}
	if got := rc.GetString("extra"); got != "val" {
		t.Errorf("new key after replace: got %q, want %q", got, "val")
	}
}

func TestRemoteConfig_GetBool(t *testing.T) {
	rc := NewRemoteConfig(map[string]string{"enabled": "true", "disabled": "false"})
	if !rc.GetBool("enabled") {
		t.Error("GetBool(enabled) should be true")
	}
	if rc.GetBool("disabled") {
		t.Error("GetBool(disabled) should be false")
	}
	if rc.GetBool("missing") {
		t.Error("GetBool(missing) should be false")
	}
}

func TestRemoteConfig_GetInt64(t *testing.T) {
	rc := NewRemoteConfig(map[string]string{"big": "9999999999"})
	if got := rc.GetInt64("big"); got != 9999999999 {
		t.Errorf("GetInt64 = %d, want %d", got, 9999999999)
	}
}

func TestRemoteConfig_GetStringSlice(t *testing.T) {
	rc := NewRemoteConfig(map[string]string{"tags": "a, b, c"})
	got := rc.GetStringSlice("tags")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("GetStringSlice = %v, want [a b c]", got)
	}
	if slice := rc.GetStringSlice("missing"); slice != nil {
		t.Errorf("GetStringSlice(missing) = %v, want nil", slice)
	}
}

func TestRemoteConfig_GetDuration_Milliseconds(t *testing.T) {
	rc := NewRemoteConfig(map[string]string{"timeout": "3000"})
	if got := rc.GetDuration("timeout"); got != 3*time.Second {
		t.Errorf("numeric duration = %v, want 3s", got)
	}
}

func TestRemoteConfig_MergeFile_Noop(t *testing.T) {
	rc := NewRemoteConfig(nil)
	if err := rc.MergeFile("/any/path"); err != nil {
		t.Errorf("MergeFile should be no-op: %v", err)
	}
}
