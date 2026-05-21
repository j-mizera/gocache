package persistence_test

import (
	"context"
	"strings"
	"testing"
	"time"

	apiconfig "gocache/api/config"
	apipersistence "gocache/api/persistence"
)

// fakeProvider is a test-only SnapshotProvider. Build returns a stub
// Source/Snapshotter — registry tests don't exercise their behaviour,
// only that registration and lookup correctly route the provider.
type fakeProvider struct {
	name      string
	buildErr  error
	buildCall int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Build(_ apiconfig.PluginConfig) (apipersistence.Source, apipersistence.Snapshotter, error) {
	f.buildCall++
	if f.buildErr != nil {
		return nil, nil, f.buildErr
	}
	return &fakeSource{}, &fakeSnap{}, nil
}

// noopConfig is a stand-in PluginConfig for registry tests — they
// don't exercise key reads, just routing.
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
	apipersistence.ResetSnapshotProviderForTest()
	if got := apipersistence.SnapshotProviderRegistered(); got != nil {
		t.Errorf("expected nil before any registration, got %v", got)
	}
}

func TestRegistry_Register_RoundTrip(t *testing.T) {
	apipersistence.ResetSnapshotProviderForTest()
	t.Cleanup(apipersistence.ResetSnapshotProviderForTest)

	p := &fakeProvider{name: "test-provider"}
	apipersistence.RegisterSnapshotProvider(p)

	got := apipersistence.SnapshotProviderRegistered()
	if got == nil {
		t.Fatal("registered provider not returned")
	}
	if got.Name() != "test-provider" {
		t.Errorf("name = %q, want test-provider", got.Name())
	}

	src, snap, err := got.Build(noopConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if src == nil || snap == nil {
		t.Errorf("Build returned nils: src=%v snap=%v", src, snap)
	}
	if p.buildCall != 1 {
		t.Errorf("buildCall = %d, want 1", p.buildCall)
	}
}

func TestRegistry_DoubleRegister_Panics(t *testing.T) {
	apipersistence.ResetSnapshotProviderForTest()
	t.Cleanup(apipersistence.ResetSnapshotProviderForTest)

	apipersistence.RegisterSnapshotProvider(&fakeProvider{name: "first"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on double-register, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not a string: %T", r)
		}
		// Both names should appear so misconfiguration is obvious.
		if !strings.Contains(msg, "first") || !strings.Contains(msg, "second") {
			t.Errorf("panic message missing names: %s", msg)
		}
	}()
	apipersistence.RegisterSnapshotProvider(&fakeProvider{name: "second"})
}

func TestRegistry_RegisterNil_Panics(t *testing.T) {
	apipersistence.ResetSnapshotProviderForTest()
	t.Cleanup(apipersistence.ResetSnapshotProviderForTest)

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil provider, got none")
		}
	}()
	apipersistence.RegisterSnapshotProvider(nil)
}

// --- AOF provider registry tests ---

type fakeAOFProvider struct {
	name     string
	buildErr error
}

func (f *fakeAOFProvider) Name() string { return f.name }
func (f *fakeAOFProvider) Build(_ apiconfig.PluginConfig) (apipersistence.Source, apipersistence.Sink, error) {
	if f.buildErr != nil {
		return nil, nil, f.buildErr
	}
	return &fakeSource{}, &fakeSink{}, nil
}

type fakeSink struct{}

func (*fakeSink) Name() string                                                      { return "fake-sink" }
func (*fakeSink) FsyncPolicy() apipersistence.FsyncPolicy                           { return apipersistence.FsyncNo }
func (*fakeSink) Apply(_ context.Context, _ []apipersistence.Mutation) error         { return nil }
func (*fakeSink) Close(_ context.Context) error                                      { return nil }

func TestAOFRegistry_NoProvider_ReturnsNil(t *testing.T) {
	apipersistence.ResetAOFProviderForTest()
	if got := apipersistence.AOFProviderRegistered(); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestAOFRegistry_Register_RoundTrip(t *testing.T) {
	apipersistence.ResetAOFProviderForTest()
	t.Cleanup(apipersistence.ResetAOFProviderForTest)

	p := &fakeAOFProvider{name: "test-aof"}
	apipersistence.RegisterAOFProvider(p)

	got := apipersistence.AOFProviderRegistered()
	if got == nil {
		t.Fatal("registered provider not returned")
	}
	if got.Name() != "test-aof" {
		t.Errorf("name = %q, want test-aof", got.Name())
	}

	src, sink, err := got.Build(noopConfig{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if src == nil || sink == nil {
		t.Errorf("Build returned nils: src=%v sink=%v", src, sink)
	}
}

func TestAOFRegistry_DoubleRegister_Panics(t *testing.T) {
	apipersistence.ResetAOFProviderForTest()
	t.Cleanup(apipersistence.ResetAOFProviderForTest)

	apipersistence.RegisterAOFProvider(&fakeAOFProvider{name: "first"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on double-register, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not a string: %T", r)
		}
		if !strings.Contains(msg, "first") || !strings.Contains(msg, "second") {
			t.Errorf("panic message missing names: %s", msg)
		}
	}()
	apipersistence.RegisterAOFProvider(&fakeAOFProvider{name: "second"})
}

func TestAOFRegistry_RegisterNil_Panics(t *testing.T) {
	apipersistence.ResetAOFProviderForTest()
	t.Cleanup(apipersistence.ResetAOFProviderForTest)

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil provider, got none")
		}
	}()
	apipersistence.RegisterAOFProvider(nil)
}
