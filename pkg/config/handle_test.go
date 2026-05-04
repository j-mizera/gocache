package config

import (
	"sync"
	"testing"

	"github.com/spf13/viper"

	apiconfig "gocache/api/config"
)

// Note: these tests reach for unexported helpers (installHandle,
// serverConfig, fireReload, resetReloadHooksForTest) so they live in
// the config package proper rather than config_test.

func TestPluginConfigFor_NilBeforeLoad(t *testing.T) {
	prev := serverConfig.Load()
	t.Cleanup(func() { serverConfig.Store(prev) })

	serverConfig.Store(nil)
	got := PluginConfigFor("anything")
	if got == nil {
		t.Fatal("PluginConfigFor returned nil")
	}
	if _, ok := got.(nopConfig); !ok {
		t.Errorf("expected nopConfig before Load, got %T", got)
	}
	if got.GetString("file") != "" {
		t.Errorf("nopConfig.GetString must return zero value")
	}
	if got.IsSet("file") {
		t.Errorf("nopConfig.IsSet must return false")
	}
}

func TestPluginConfigFor_AfterInstall(t *testing.T) {
	prev := serverConfig.Load()
	t.Cleanup(func() { serverConfig.Store(prev) })

	v := viper.New()
	v.Set("plugins.config.test-plugin.file", "snapshot.dat")
	v.Set("plugins.config.test-plugin.flush", "5s")
	installHandle(v)

	cfg := PluginConfigFor("test-plugin")
	if cfg.GetString("file") != "snapshot.dat" {
		t.Errorf("scoped read failed: got %q", cfg.GetString("file"))
	}
	if cfg.GetDuration("flush").String() != "5s" {
		t.Errorf("duration read failed: got %v", cfg.GetDuration("flush"))
	}
	if !cfg.IsSet("file") {
		t.Errorf("IsSet failed for set key")
	}
	if cfg.IsSet("missing") {
		t.Errorf("IsSet returned true for unset key")
	}
}

func TestPluginConfigFor_SetDefaultPropagates(t *testing.T) {
	prev := serverConfig.Load()
	t.Cleanup(func() { serverConfig.Store(prev) })

	v := viper.New()
	installHandle(v)

	cfg := PluginConfigFor("test-plugin")
	cfg.SetDefault("file", "default.dat")

	if got := cfg.GetString("file"); got != "default.dat" {
		t.Errorf("SetDefault not honoured: got %q", got)
	}
	// The default should land at the fully qualified key inside viper.
	if got := v.GetString("plugins.config.test-plugin.file"); got != "default.dat" {
		t.Errorf("default not stored under qualified key: got %q", got)
	}
}

func TestConfigFileUsed_EmptyBeforeLoad(t *testing.T) {
	prev := serverConfig.Load()
	t.Cleanup(func() { serverConfig.Store(prev) })

	serverConfig.Store(nil)
	if got := ConfigFileUsed(); got != "" {
		t.Errorf("expected empty string before Load, got %q", got)
	}
}

func TestOnReload_FanOut(t *testing.T) {
	resetReloadHooksForTest()
	t.Cleanup(resetReloadHooksForTest)

	var (
		mu   sync.Mutex
		hits []string
	)
	OnReload(func() {
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, "a")
	})
	OnReload(func() {
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, "b")
	})

	fireReload()

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0] != "a" || hits[1] != "b" {
		t.Errorf("hits = %v, want [a b]", hits)
	}
}

func TestOnPluginReload_DeliversFreshScopedView(t *testing.T) {
	resetReloadHooksForTest()
	t.Cleanup(resetReloadHooksForTest)

	prev := serverConfig.Load()
	t.Cleanup(func() { serverConfig.Store(prev) })

	v := viper.New()
	v.Set("plugins.config.test-plugin.file", "first.dat")
	installHandle(v)

	var got string
	OnPluginReload("test-plugin", func(cfg apiconfig.PluginConfig) {
		got = cfg.GetString("file")
	})

	// First reload should observe "first.dat".
	fireReload()
	if got != "first.dat" {
		t.Errorf("first reload: got %q, want %q", got, "first.dat")
	}

	// Mutate underlying value and reload again — handler should see fresh value.
	v.Set("plugins.config.test-plugin.file", "second.dat")
	fireReload()
	if got != "second.dat" {
		t.Errorf("second reload: got %q, want %q", got, "second.dat")
	}
}

func TestOnPluginReload_NopWhenHandleUnset(t *testing.T) {
	resetReloadHooksForTest()
	t.Cleanup(resetReloadHooksForTest)

	prev := serverConfig.Load()
	t.Cleanup(func() { serverConfig.Store(prev) })
	serverConfig.Store(nil)

	var observed apiconfig.PluginConfig
	OnPluginReload("test-plugin", func(cfg apiconfig.PluginConfig) {
		observed = cfg
	})
	fireReload()

	if _, ok := observed.(nopConfig); !ok {
		t.Errorf("expected nopConfig when handle unset, got %T", observed)
	}
}

func TestOnReload_RegistrationDuringFanOutDoesNotRace(t *testing.T) {
	resetReloadHooksForTest()
	t.Cleanup(resetReloadHooksForTest)

	var firstFired, secondFired bool
	OnReload(func() {
		firstFired = true
		OnReload(func() {
			secondFired = true
		})
	})

	fireReload()
	if !firstFired {
		t.Error("first callback did not fire")
	}
	if secondFired {
		t.Error("second callback fired during the same round it was registered")
	}

	fireReload()
	if !secondFired {
		t.Error("second callback did not fire on the next round")
	}
}
