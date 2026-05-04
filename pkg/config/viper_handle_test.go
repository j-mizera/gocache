package config

import (
	"sync"
	"testing"

	"github.com/spf13/viper"
)

// Note: these tests use unexported helpers (setServerViper, fireReload,
// resetReloadHooksForTest) so they live in the config package proper
// rather than config_test.

func TestViper_NilBeforeSet(t *testing.T) {
	// Snapshot+restore the global so test order doesn't matter.
	prev := serverViper.Load()
	t.Cleanup(func() { serverViper.Store(prev) })

	serverViper.Store(nil)
	if got := Viper(); got != nil {
		t.Errorf("expected nil before setServerViper, got %v", got)
	}
}

func TestViper_RoundTrip(t *testing.T) {
	prev := serverViper.Load()
	t.Cleanup(func() { serverViper.Store(prev) })

	v := viper.New()
	setServerViper(v)
	if got := Viper(); got != v {
		t.Errorf("Viper() = %v, want %v", got, v)
	}
}

func TestOnReload_FanOut(t *testing.T) {
	resetReloadHooksForTest()
	t.Cleanup(resetReloadHooksForTest)

	var (
		mu    sync.Mutex
		hits  []string
	)
	OnReload(func(_ *viper.Viper) {
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, "a")
	})
	OnReload(func(_ *viper.Viper) {
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, "b")
	})

	fireReload(viper.New())

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0] != "a" || hits[1] != "b" {
		t.Errorf("hits = %v, want [a b]", hits)
	}
}

func TestOnReload_PassesViper(t *testing.T) {
	resetReloadHooksForTest()
	t.Cleanup(resetReloadHooksForTest)

	v := viper.New()
	v.Set("test.key", "test-value")

	var got string
	OnReload(func(rv *viper.Viper) {
		got = rv.GetString("test.key")
	})
	fireReload(v)
	if got != "test-value" {
		t.Errorf("callback did not receive viper instance: got %q", got)
	}
}

func TestOnReload_RegistrationDuringFanOutDoesNotRace(t *testing.T) {
	// A callback that registers another callback should not race or
	// reorder — fireReload snapshots the slice before iterating.
	resetReloadHooksForTest()
	t.Cleanup(resetReloadHooksForTest)

	var firstFired, secondFired bool
	OnReload(func(_ *viper.Viper) {
		firstFired = true
		// Register a second callback during the fan-out.
		OnReload(func(_ *viper.Viper) {
			secondFired = true
		})
	})

	fireReload(viper.New())
	if !firstFired {
		t.Error("first callback did not fire")
	}
	// Second callback should NOT have fired this round — registered
	// after the snapshot was taken. It will fire on the next reload.
	if secondFired {
		t.Error("second callback fired during the same round it was registered")
	}

	fireReload(viper.New())
	if !secondFired {
		t.Error("second callback did not fire on the next round")
	}
}
