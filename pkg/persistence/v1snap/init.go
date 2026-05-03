package v1snap

import (
	apipersistence "gocache/api/persistence"
)

// init registers v1snap as the embedded snapshot plugin per ADR-0007.
// cmd/server/main.go blank-imports this package to wire the v1 backend
// into the server; resolution happens via
// api/persistence.SnapshotProviderRegistered.
//
// A second persistence plugin compiled into the same binary will panic
// here at init time — see the registration contract.
func init() {
	apipersistence.RegisterSnapshotProvider(&provider{})
}

// provider is the bridge between v1snap's concrete Source / Snapshotter
// types and the api/persistence.SnapshotProvider interface. It owns
// nothing beyond the Build method — instances live as long as init.
type provider struct{}

// Name implements api/persistence.SnapshotProvider. Stable identifier
// used for boot-time logs.
func (provider) Name() string { return "v1-snapshot" }

// Build implements api/persistence.SnapshotProvider. The returned
// setFilename closure routes the new path to both halves so config
// reload in main.go updates the boot-side and runtime-save-side
// filename atomically (boot reads at startup; save reads at every
// tick, so the runtime path is the one that matters in steady state).
func (provider) Build(filename string) (apipersistence.Source, apipersistence.Snapshotter, func(string)) {
	src := NewSource(filename)
	snap := NewSnapshotter(filename)
	setFilename := func(f string) {
		src.SetFilename(f)
		snap.SetFilename(f)
	}
	return src, snap, setFilename
}
