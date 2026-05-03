package persistence

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
)

func TestCoordinator_BootInto_NilSource(t *testing.T) {
	c := New(nil)
	target := cache.New()

	lsn, err := c.BootInto(context.Background(), target)
	if err != nil {
		t.Fatalf("BootInto with nil source: unexpected error: %v", err)
	}
	if lsn != apipersistence.ZeroLSN {
		t.Errorf("BootInto with nil source: lsn = %d, want %d", lsn, apipersistence.ZeroLSN)
	}
	if got := target.Len(); got != 0 {
		t.Errorf("BootInto with nil source: target.Len = %d, want 0", got)
	}
}

func TestCoordinator_BootInto_GobRoundTrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "snapshot.dat")

	src := cache.New()
	src.Lock()
	_ = src.RawSet(context.Background(), "str", "hello", 0)
	_ = src.RawSet(context.Background(), "list", []string{"a", "b", "c"}, 0)
	_ = src.RawSet(context.Background(), "hash", map[string]string{"k": "v"}, 0)
	src.Unlock()

	if err := SaveSnapshot(context.Background(), file, src); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	target := cache.New()
	co := New(NewGobSource(file))

	lsn, err := co.BootInto(context.Background(), target)
	if err != nil {
		t.Fatalf("BootInto: %v", err)
	}
	if lsn != apipersistence.ZeroLSN {
		// Gob shim has no LSN concept, so the recovered LSN is zero.
		t.Errorf("BootInto: lsn = %d, want %d (gob shim is LSN-naive)", lsn, apipersistence.ZeroLSN)
	}
	if got := target.Len(); got != 3 {
		t.Errorf("target.Len = %d, want 3", got)
	}

	target.Lock()
	defer target.Unlock()

	entry, ok := target.RawGet("str")
	if !ok {
		t.Fatal("str not found in recovered cache")
	}
	if got := string(target.ResolvePacked(entry)); got != "hello" {
		t.Errorf("recovered str = %q, want %q", got, "hello")
	}
}

func TestCoordinator_BootInto_MissingFileTreatedAsInitial(t *testing.T) {
	target := cache.New()
	co := New(NewGobSource("/nonexistent/path/snapshot.dat"))

	lsn, err := co.BootInto(context.Background(), target)
	if err != nil {
		t.Fatalf("BootInto on missing file: %v", err)
	}
	if lsn != apipersistence.ZeroLSN {
		t.Errorf("missing file: lsn = %d, want %d", lsn, apipersistence.ZeroLSN)
	}
	if got := target.Len(); got != 0 {
		t.Errorf("missing file: target.Len = %d, want 0", got)
	}
}

func TestCoordinator_BootInto_SkipsExpired(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "snapshot.dat")

	src := cache.New()
	src.Lock()
	_ = src.RawSet(context.Background(), "expired", "val", time.Now().Add(-time.Hour).UnixNano())
	_ = src.RawSet(context.Background(), "alive", "val", 0)
	src.Unlock()

	if err := SaveSnapshot(context.Background(), file, src); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	target := cache.New()
	co := New(NewGobSource(file))
	if _, err := co.BootInto(context.Background(), target); err != nil {
		t.Fatalf("BootInto: %v", err)
	}

	target.Lock()
	defer target.Unlock()
	if _, ok := target.RawGet("expired"); ok {
		t.Error("expected expired key to be skipped during recovery")
	}
	if _, ok := target.RawGet("alive"); !ok {
		t.Error("expected alive key to be recovered")
	}
}

func TestCoordinator_AllocateLSN_Monotonic(t *testing.T) {
	co := New(nil)
	const total = 10_000

	got := make([]uint64, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got[idx] = uint64(co.AllocateLSN())
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]struct{}, total)
	var max uint64
	for _, v := range got {
		if v == 0 {
			t.Fatal("AllocateLSN returned ZeroLSN")
		}
		if _, dup := seen[v]; dup {
			t.Fatalf("AllocateLSN returned duplicate: %d", v)
		}
		seen[v] = struct{}{}
		if v > max {
			max = v
		}
	}
	if max != uint64(total) {
		t.Errorf("max LSN = %d, want %d (no gaps after %d allocations)", max, total, total)
	}
	if got := co.CurrentLSN(); got != apipersistence.LSN(total) {
		t.Errorf("CurrentLSN = %d, want %d", got, total)
	}
}

func TestCoordinator_SetLSN(t *testing.T) {
	co := New(nil)
	co.SetLSN(42)
	if got := co.CurrentLSN(); got != 42 {
		t.Errorf("after SetLSN(42): CurrentLSN = %d, want 42", got)
	}
	if got := co.AllocateLSN(); got != 43 {
		t.Errorf("AllocateLSN after SetLSN(42) = %d, want 43", got)
	}
}

// fakeSource lets tests inject arbitrary boot results, including invalid
// ones, to exercise error paths.
type fakeSource struct {
	name string
	res  apipersistence.BootResult
	err  error
}

func (s *fakeSource) Name() string { return s.name }
func (s *fakeSource) Boot(_ context.Context) (apipersistence.BootResult, error) {
	return s.res, s.err
}

func TestCoordinator_BootInto_PropagatesSourceError(t *testing.T) {
	wantErr := errors.New("source-side failure")
	co := New(&fakeSource{name: "fake", err: wantErr})

	_, err := co.BootInto(context.Background(), cache.New())
	if err == nil {
		t.Fatal("expected error from BootInto")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("BootInto error = %v, want wraps %v", err, wantErr)
	}
}

func TestCoordinator_BootInto_InvalidBootMode(t *testing.T) {
	co := New(&fakeSource{
		name: "fake",
		res:  apipersistence.BootResult{Mode: 99},
	})

	_, err := co.BootInto(context.Background(), cache.New())
	if err == nil {
		t.Fatal("expected error for invalid BootMode")
	}
	if !errors.Is(err, apipersistence.ErrInvalidBootMode) {
		t.Errorf("BootInto error = %v, want wraps ErrInvalidBootMode", err)
	}
}

func TestCoordinator_BootInto_SnapshotModeRequiresIterator(t *testing.T) {
	co := New(&fakeSource{
		name: "fake",
		res:  apipersistence.BootResult{Mode: apipersistence.BootModeSnapshot}, // Snapshot nil
	})

	_, err := co.BootInto(context.Background(), cache.New())
	if err == nil {
		t.Fatal("expected error for nil Snapshot iterator under BootModeSnapshot")
	}
}

func TestCoordinator_BootInto_ReplayAdvancesLSNCursor(t *testing.T) {
	mutations := []apipersistence.Mutation{
		{LSN: 10, Op: "SET", Key: "a"},
		{LSN: 20, Op: "SET", Key: "b"},
		{LSN: 30, Op: "DEL", Key: "a"},
	}
	co := New(&fakeSource{
		name: "fake",
		res: apipersistence.BootResult{
			Mode:   apipersistence.BootModeReplay,
			Replay: &sliceReplay{items: mutations},
		},
	})

	lsn, err := co.BootInto(context.Background(), cache.New())
	if err != nil {
		t.Fatalf("BootInto: %v", err)
	}
	if lsn != 30 {
		t.Errorf("recovered LSN = %d, want 30 (highest in replay)", lsn)
	}
	if got := co.CurrentLSN(); got != 30 {
		t.Errorf("CurrentLSN after replay = %d, want 30", got)
	}
}

// sliceReplay is a ReplayIterator backed by an in-memory slice.
type sliceReplay struct {
	items  []apipersistence.Mutation
	idx    int
	closed bool
}

func (r *sliceReplay) Next(_ context.Context) (apipersistence.Mutation, error) {
	if r.idx >= len(r.items) {
		return apipersistence.Mutation{}, io.EOF
	}
	m := r.items[r.idx]
	r.idx++
	return m, nil
}

func (r *sliceReplay) Close() error {
	r.closed = true
	return nil
}

func TestGobSource_Name(t *testing.T) {
	if got := NewGobSource("x").Name(); got != "gob-snapshot" {
		t.Errorf("GobSource.Name = %q, want %q", got, "gob-snapshot")
	}
}

func TestGobSource_BootEmptyFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "empty.dat")
	if err := writeFile(file, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}

	res, err := NewGobSource(file).Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot on empty file: %v", err)
	}
	if res.Mode != apipersistence.BootModeInitial {
		t.Errorf("empty file mode = %v, want Initial (treated as no-state)", res.Mode)
	}
	_ = res.Close()
}

// writeFile is a helper that creates a file with given bytes (may be nil
// for empty). Avoids importing os in test-only paths and keeps the table-
// driven tests clean.
func writeFile(path string, data []byte) error {
	return writeFileFn(path, data)
}
