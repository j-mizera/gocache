package persistence_test

import (
	"context"
	"strings"
	"testing"

	apipersistence "gocache/api/persistence"
)

// fakeProvider is a test-only SnapshotProvider that records its name
// and produces nil Source/Snapshotter — the registry tests don't
// exercise Build's outputs, only that the right provider is returned
// from the lookup.
type fakeProvider struct {
	name string
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Build(_ string) (apipersistence.Source, apipersistence.Snapshotter, func(string)) {
	return nil, nil, func(string) {}
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

func TestRegistry_BuildPlumbing(t *testing.T) {
	// Sanity check that Build's signature is wired through correctly
	// by exercising the round-trip with a non-fake provider built in
	// the same pattern v1snap uses.
	apipersistence.ResetSnapshotProviderForTest()
	t.Cleanup(apipersistence.ResetSnapshotProviderForTest)

	apipersistence.RegisterSnapshotProvider(&buildableProvider{name: "buildable"})
	got := apipersistence.SnapshotProviderRegistered()
	if got == nil {
		t.Fatal("provider not registered")
	}
	src, snap, setFile := got.Build("/tmp/test-file")
	if src == nil || snap == nil {
		t.Fatalf("Build returned nil pieces: src=%v snap=%v", src, snap)
	}
	// setFile should not panic on call
	setFile("/tmp/other")
	// Source.Boot should work with the new filename — buildableProvider's
	// fake source records what filename was passed.
	bs := src.(*buildableSource)
	if bs.lastFilename != "/tmp/other" {
		t.Errorf("setFile did not update source: lastFilename=%q", bs.lastFilename)
	}
}

// buildableProvider exercises Build's three-return-value contract end
// to end. The Source/Snapshotter it returns are fakes that capture
// state so the test can assert the setFilename closure actually
// reaches them.
type buildableProvider struct {
	name string
}

func (b *buildableProvider) Name() string { return b.name }

func (b *buildableProvider) Build(filename string) (apipersistence.Source, apipersistence.Snapshotter, func(string)) {
	src := &buildableSource{lastFilename: filename}
	snap := &buildableSnapshotter{lastFilename: filename}
	return src, snap, func(f string) {
		src.lastFilename = f
		snap.lastFilename = f
	}
}

type buildableSource struct {
	lastFilename string
}

func (b *buildableSource) Name() string { return "buildable-source" }
func (b *buildableSource) Boot(_ context.Context) (apipersistence.BootResult, error) {
	return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
}

type buildableSnapshotter struct {
	lastFilename string
}

func (b *buildableSnapshotter) Name() string { return "buildable-snap" }
func (b *buildableSnapshotter) SaveSnapshot(_ context.Context, _ apipersistence.SnapshotSource) error {
	return nil
}
