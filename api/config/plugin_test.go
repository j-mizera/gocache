package config_test

import (
	"testing"

	"gocache/api/config"
)

func TestPluginConfigFor_BeforeProvider(t *testing.T) {
	cfg := config.PluginConfigFor("anything")
	if cfg == nil {
		t.Fatal("PluginConfigFor returned nil before provider set")
	}
	if cfg.GetString("key") != "" {
		t.Error("expected empty string from fallback config")
	}
}

func TestPluginConfigFor_AfterProvider(t *testing.T) {
	config.SetPluginConfigProvider(func(name string) config.PluginConfig {
		m := config.NewMapConfig()
		m.Values["name"] = name
		return m
	})
	t.Cleanup(func() { config.SetPluginConfigProvider(nil) })

	cfg := config.PluginConfigFor("test-plugin")
	if got := cfg.GetString("name"); got != "test-plugin" {
		t.Errorf("provider not called: got %q, want %q", got, "test-plugin")
	}
}
