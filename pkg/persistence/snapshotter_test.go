package persistence

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
)

// recordingSnapshotter drains the SnapshotSource into memory so tests can
// assert which entries the coordinator passed in. Threadsafe so concurrent
// SaveSnapshot calls (none today, but cheap insurance) don't race.
type recordingSnapshotter struct {
	name string

	mu      sync.Mutex
	calls   int
	entries []apipersistence.SnapshotEntry
	saveErr error
}

func (r *recordingSnapshotter) Name() string { return r.name }

func (r *recordingSnapshotter) SaveSnapshot(ctx context.Context, src apipersistence.SnapshotSource) error {
	r.mu.Lock()
	r.calls++
	if r.saveErr != nil {
		err := r.saveErr
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	for {
		e, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.entries = append(r.entries, e)
		r.mu.Unlock()
	}
}

func (r *recordingSnapshotter) snapshotEntries() []apipersistence.SnapshotEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]apipersistence.SnapshotEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func TestCoordinator_Snapshot_NoSnapshotter(t *testing.T) {
	c := New(nil)
	target := cache.New()

	err := c.Snapshot(context.Background(), target)
	if !errors.Is(err, apipersistence.ErrNoSnapshotter) {
		t.Errorf("expected ErrNoSnapshotter, got %v", err)
	}
}

func TestCoordinator_RegisterSnapshotter_GetAndClear(t *testing.T) {
	c := New(nil)
	rec := &recordingSnapshotter{name: "rec"}

	if got := c.Snapshotter(); got != nil {
		t.Errorf("Snapshotter() before register: got %v, want nil", got)
	}
	c.RegisterSnapshotter(rec)
	if got := c.Snapshotter(); got != rec {
		t.Errorf("Snapshotter() after register: got %v, want %v", got, rec)
	}
	c.RegisterSnapshotter(nil)
	if got := c.Snapshotter(); got != nil {
		t.Errorf("Snapshotter() after clear: got %v, want nil", got)
	}
}

func TestCoordinator_Snapshot_FeedsAllEntries(t *testing.T) {
	target := cache.New()
	target.Lock()
	_ = target.RawSet(context.Background(), "k1", "v1", 0)
	_ = target.RawSet(context.Background(), "k2", "v2", 0)
	_ = target.RawSet(context.Background(), "k3", "v3", 0)
	target.Unlock()

	rec := &recordingSnapshotter{name: "rec"}
	c := New(nil)
	c.RegisterSnapshotter(rec)

	if err := c.Snapshot(context.Background(), target); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	entries := rec.snapshotEntries()
	if len(entries) != 3 {
		t.Fatalf("entries: got %d, want 3", len(entries))
	}
	keys := map[string]bool{}
	for _, e := range entries {
		keys[e.Key] = true
	}
	for _, k := range []string{"k1", "k2", "k3"} {
		if !keys[k] {
			t.Errorf("missing key %s in snapshot", k)
		}
	}
	if rec.calls != 1 {
		t.Errorf("calls = %d, want 1", rec.calls)
	}
}

func TestCoordinator_Snapshot_PropagatesError(t *testing.T) {
	target := cache.New()
	target.Lock()
	_ = target.RawSet(context.Background(), "k", "v", 0)
	target.Unlock()

	wantErr := errors.New("disk full")
	rec := &recordingSnapshotter{name: "rec", saveErr: wantErr}
	c := New(nil)
	c.RegisterSnapshotter(rec)

	err := c.Snapshot(context.Background(), target)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}

func TestCoordinator_Snapshot_GobRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "snap.dat")

	source := cache.New()
	source.Lock()
	_ = source.RawSet(context.Background(), "str", "hello", 0)
	_ = source.RawSet(context.Background(), "list", []string{"a", "b"}, 0)
	source.Unlock()

	gob := NewGobSource(file)
	c := New(gob)
	c.RegisterSnapshotter(gob)

	if err := c.Snapshot(context.Background(), source); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("snapshot file not created: %v", err)
	}

	// Round-trip: load the same snapshot back into a fresh cache.
	target := cache.New()
	if _, err := c.BootInto(context.Background(), target); err != nil {
		t.Fatalf("BootInto: %v", err)
	}
	if got := target.Len(); got != 2 {
		t.Errorf("target.Len = %d, want 2", got)
	}
}

func TestGobSource_SetFilename_HotReload(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "a.dat")
	file2 := filepath.Join(dir, "b.dat")

	source := cache.New()
	source.Lock()
	_ = source.RawSet(context.Background(), "k", "v", 0)
	source.Unlock()

	gob := NewGobSource(file1)
	c := New(gob)
	c.RegisterSnapshotter(gob)

	if err := c.Snapshot(context.Background(), source); err != nil {
		t.Fatalf("Snapshot to file1: %v", err)
	}
	if _, err := os.Stat(file1); err != nil {
		t.Fatalf("file1 not created: %v", err)
	}

	gob.SetFilename(file2)
	if err := c.Snapshot(context.Background(), source); err != nil {
		t.Fatalf("Snapshot to file2: %v", err)
	}
	if _, err := os.Stat(file2); err != nil {
		t.Fatalf("file2 not created: %v", err)
	}

	// file1 still exists from the first save — SetFilename doesn't unlink.
	if _, err := os.Stat(file1); err != nil {
		t.Errorf("file1 disappeared after SetFilename: %v", err)
	}
}

func TestSliceSnapshotSource_EOF(t *testing.T) {
	entries := []apipersistence.SnapshotEntry{
		{Key: "a"}, {Key: "b"}, {Key: "c"},
	}
	src := &sliceSnapshotSource{entries: entries}

	for i := range entries {
		got, err := src.Next(context.Background())
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		if got.Key != entries[i].Key {
			t.Errorf("Next #%d: got %s, want %s", i, got.Key, entries[i].Key)
		}
	}
	_, err := src.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("Next after exhausted: got %v, want io.EOF", err)
	}
	// Second call after EOF still EOF — single-pass forward-only.
	_, err = src.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("Next twice after EOF: got %v, want io.EOF", err)
	}
}

func TestGobSource_SaveSnapshot_EmptyFilename(t *testing.T) {
	gob := NewGobSource("")
	rec := &recordingSnapshotter{}
	_ = rec // unused, just to exercise the path through SaveSnapshot
	src := &sliceSnapshotSource{entries: []apipersistence.SnapshotEntry{{Key: "k"}}}
	err := gob.SaveSnapshot(context.Background(), src)
	if err == nil {
		t.Error("SaveSnapshot with empty filename: expected error, got nil")
	}
}

// Compile-time check: the recording test snapshotter satisfies the
// api/persistence contract. Catches contract drift at build time.
var _ apipersistence.Snapshotter = (*recordingSnapshotter)(nil)

// recordingLSNSnapshotter implements both Snapshotter and LSNSeeder so
// tests can assert that the coordinator threads the LSN cursor through
// before delegating to SaveSnapshot.
type recordingLSNSnapshotter struct {
	recordingSnapshotter
	seededLSN apipersistence.LSN
}

func (r *recordingLSNSnapshotter) SetLSN(lsn apipersistence.LSN) {
	r.seededLSN = lsn
}

func TestCoordinator_Snapshot_SeedsLSN(t *testing.T) {
	target := cache.New()
	target.Lock()
	_ = target.RawSet(context.Background(), "k", "v", 0)
	target.Unlock()

	rec := &recordingLSNSnapshotter{recordingSnapshotter: recordingSnapshotter{name: "v1-mock"}}
	c := New(nil)
	c.RegisterSnapshotter(rec)
	// Burn LSNs so CurrentLSN is non-zero — verifies the coordinator
	// reads its own cursor instead of always passing ZeroLSN.
	c.AllocateLSN()
	c.AllocateLSN()
	c.AllocateLSN()

	if err := c.Snapshot(context.Background(), target); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if rec.seededLSN != apipersistence.LSN(3) {
		t.Errorf("seeded LSN = %d, want 3", rec.seededLSN)
	}
}

func TestCoordinator_Snapshot_NoSeedWhenNoLSNSeeder(t *testing.T) {
	// Plain Snapshotter (no LSNSeeder) should not be seeded — the
	// coordinator's type assertion fails silently.
	target := cache.New()
	target.Lock()
	_ = target.RawSet(context.Background(), "k", "v", 0)
	target.Unlock()

	rec := &recordingSnapshotter{name: "gob-mock"}
	c := New(nil)
	c.RegisterSnapshotter(rec)
	c.AllocateLSN() // make CurrentLSN > 0
	if err := c.Snapshot(context.Background(), target); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// No assertion on seededLSN — the recording snapshotter doesn't
	// have one. Compiling without a panic is the test.
}
