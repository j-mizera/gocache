package plugincfg_test

import (
	"testing"

	"gocache/api/config"
	"gocache/commons/plugincfg"
)

func TestPluginConfigFor_BeforeProvider(t *testing.T) {
	cfg := plugincfg.PluginConfigFor("anything")
	if cfg == nil {
		t.Fatal("PluginConfigFor returned nil before provider set")
	}
	if cfg.GetString("key") != "" {
		t.Error("expected empty string from fallback config")
	}
}

func TestPluginConfigFor_AfterProvider(t *testing.T) {
	plugincfg.SetPluginConfigProvider(func(name string) config.PluginConfig {
		m := plugincfg.NewMapConfig()
		m.Values["name"] = name
		return m
	})
	t.Cleanup(func() { plugincfg.SetPluginConfigProvider(nil) })

	cfg := plugincfg.PluginConfigFor("test-plugin")
	if got := cfg.GetString("name"); got != "test-plugin" {
		t.Errorf("provider not called: got %q, want %q", got, "test-plugin")
	}
}
