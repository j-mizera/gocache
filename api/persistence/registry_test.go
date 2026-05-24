package persistence_test

import (
	"context"
	"strings"
	"testing"
	"time"

	apiconfig "gocache/api/config"
	apipersistence "gocache/api/persistence"
)

type fakeProvider struct {
	name      string
	buildErr  error
	buildCall int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Build(_ apiconfig.PluginConfig, _ apipersistence.CacheStore) (*apipersistence.Backend, error) {
	f.buildCall++
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	return &apipersistence.Backend{
		Source:      &fakeSource{},
		Snapshotter: &fakeSnap{},
	}, nil
}

type noopConfig struct{}

func (noopConfig) GetString(string) string          { return "" }
func (noopConfig) GetInt(string) int                { return 0 }
func (noopConfig) GetInt64(string) int64            { return 0 }
func (noopConfig) GetBool(string) bool              { return false }
func (noopConfig) GetDuration(string) time.Duration { return 0 }
func (noopConfig) GetStringSlice(string) []string   { return nil }
func (noopConfig) IsSet(string) bool                { return false }
func (noopConfig) SetDefault(string, any)           {}

type fakeSource struct{}

func (*fakeSource) Name() string { return "fake-src" }
func (*fakeSource) Boot(_ context.Context) (apipersistence.BootResult, error) {
	return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
}

type fakeSnap struct{}

func (*fakeSnap) Name() string { return "fake-snap" }
func (*fakeSnap) SaveSnapshot(_ context.Context, _ apipersistence.SnapshotSource) error {
	return nil
}

func TestRegistry_NoProvider_ReturnsNil(t *testing.T) {
	apipersistence.ResetProvidersForTest()
	if got := apipersistence.RegisteredProviders(); got != nil {
		t.Errorf("expected nil before any registration, got %v", got)
	}
}

func TestRegistry_Register_RoundTrip(t *testing.T) {
	apipersistence.ResetProvidersForTest()
	t.Cleanup(apipersistence.ResetProvidersForTest)

	p := &fakeProvider{name: "test-provider"}
	apipersistence.RegisterProvider(p)

	providers := apipersistence.RegisteredProviders()
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(providers))
	}
	got := providers[0]
	if got.Name() != "test-provider" {
		t.Errorf("name = %q, want test-provider", got.Name())
	}

	backend, err := got.Build(noopConfig{}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if backend.Source == nil || backend.Snapshotter == nil {
		t.Errorf("Build returned nil fields: Source=%v Snapshotter=%v", backend.Source, backend.Snapshotter)
	}
	if p.buildCall != 1 {
		t.Errorf("buildCall = %d, want 1", p.buildCall)
	}
}

func TestRegistry_MultipleProviders(t *testing.T) {
	apipersistence.ResetProvidersForTest()
	t.Cleanup(apipersistence.ResetProvidersForTest)

	apipersistence.RegisterProvider(&fakeProvider{name: "snap"})
	apipersistence.RegisterProvider(&fakeProvider{name: "aof"})

	providers := apipersistence.RegisteredProviders()
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
	if providers[0].Name() != "snap" || providers[1].Name() != "aof" {
		t.Errorf("providers = [%s, %s], want [snap, aof]", providers[0].Name(), providers[1].Name())
	}
}

func TestRegistry_DuplicateName_Panics(t *testing.T) {
	apipersistence.ResetProvidersForTest()
	t.Cleanup(apipersistence.ResetProvidersForTest)

	apipersistence.RegisterProvider(&fakeProvider{name: "dup"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate name, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not a string: %T", r)
		}
		if !strings.Contains(msg, "dup") {
			t.Errorf("panic message missing name: %s", msg)
		}
	}()
	apipersistence.RegisterProvider(&fakeProvider{name: "dup"})
}

func TestRegistry_RegisterNil_Panics(t *testing.T) {
	apipersistence.ResetProvidersForTest()
	t.Cleanup(apipersistence.ResetProvidersForTest)

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil provider, got none")
		}
	}()
	apipersistence.RegisterProvider(nil)
}
