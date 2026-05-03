package persistence

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
	"gocache/pkg/persistence/v1snap"
)

func TestDetectFormat_Gob(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "snap.dat")

	c := cache.New()
	c.Lock()
	_ = c.RawSet(context.Background(), "k", "v", 0)
	c.Unlock()
	if err := SaveSnapshot(context.Background(), file, c); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	got, err := DetectFormat(file)
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if got != FormatGob {
		t.Errorf("format = %q, want %q", got, FormatGob)
	}
}

func TestDetectFormat_V1(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "v1.snap")

	w := v1snap.NewSnapshotter(file)
	if err := w.SaveSnapshot(context.Background(), &emptySrc{}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	got, err := DetectFormat(file)
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if got != FormatV1 {
		t.Errorf("format = %q, want %q", got, FormatV1)
	}
}

func TestDetectFormat_MissingFile(t *testing.T) {
	_, err := DetectFormat(filepath.Join(t.TempDir(), "nope.dat"))
	if err == nil {
		t.Errorf("expected error on missing file, got nil")
	}
}

func TestDetectFormat_TooShort(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tiny.dat")
	// 2 bytes — can't be v1 (header is 5). Should fall back to gob.
	if err := os.WriteFile(file, []byte{0x01, 0x02}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := DetectFormat(file)
	if err != nil {
		t.Fatalf("DetectFormat: %v", err)
	}
	if got != FormatGob {
		t.Errorf("format = %q, want %q (short file should fall through to gob)", got, FormatGob)
	}
}

func TestLoadFrom_Gob(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "snap.dat")

	source := cache.New()
	source.Lock()
	_ = source.RawSet(context.Background(), "k1", "v1", 0)
	_ = source.RawSet(context.Background(), "k2", "v2", 0)
	source.Unlock()
	if err := SaveSnapshot(context.Background(), file, source); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	target := cache.New()
	if err := LoadFrom(context.Background(), file, target); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := target.Len(); got != 2 {
		t.Errorf("Len = %d, want 2", got)
	}
}

func TestLoadFrom_V1(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "v1.snap")

	w := v1snap.NewSnapshotter(file)
	src := &sliceSrc{entries: []apipersistence.SnapshotEntry{
		{Key: "a", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("alpha")},
		{Key: "b", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("beta")},
	}}
	if err := w.SaveSnapshot(context.Background(), src); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	target := cache.New()
	if err := LoadFrom(context.Background(), file, target); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := target.Len(); got != 2 {
		t.Errorf("Len = %d, want 2", got)
	}
}

func TestLoadFrom_ClearsTarget(t *testing.T) {
	// LOAD_SNAPSHOT semantics: replace target's contents with the
	// snapshot. Pre-existing entries should be cleared, not merged.
	dir := t.TempDir()
	file := filepath.Join(dir, "snap.dat")

	source := cache.New()
	source.Lock()
	_ = source.RawSet(context.Background(), "from-snap", "v", 0)
	source.Unlock()
	if err := SaveSnapshot(context.Background(), file, source); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	target := cache.New()
	target.Lock()
	_ = target.RawSet(context.Background(), "preexisting", "v", 0)
	target.Unlock()

	if err := LoadFrom(context.Background(), file, target); err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if _, ok := target.RawGet("preexisting"); ok {
		t.Errorf("preexisting key still present after LoadFrom")
	}
	if _, ok := target.RawGet("from-snap"); !ok {
		t.Errorf("from-snap key missing after LoadFrom")
	}
}

func TestLoadFrom_MissingFile(t *testing.T) {
	target := cache.New()
	err := LoadFrom(context.Background(), filepath.Join(t.TempDir(), "missing.dat"), target)
	if err == nil {
		t.Errorf("expected error for missing file")
	}
}

// emptySrc satisfies api/persistence.SnapshotSource with no entries.
// Used to write a minimal valid v1 file for DetectFormat tests.
type emptySrc struct{}

func (emptySrc) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	return apipersistence.SnapshotEntry{}, io.EOF
}

// sliceSrc is a minimal SnapshotSource for table-driven LoadFrom tests.
type sliceSrc struct {
	entries []apipersistence.SnapshotEntry
	cursor  int
}

func (s *sliceSrc) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	if s.cursor >= len(s.entries) {
		return apipersistence.SnapshotEntry{}, io.EOF
	}
	e := s.entries[s.cursor]
	s.cursor++
	return e, nil
}
