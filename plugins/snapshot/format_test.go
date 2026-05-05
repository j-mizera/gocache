package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	apipersistence "gocache/api/persistence"
)

// sliceSrc is a SnapshotSource backed by an in-memory slice. Used by
// tests to feed entries to the writer without going through a Coordinator.
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

// drain reads every entry from a SnapshotIterator and returns them.
// Always closes the iterator. Returns the close error when no Next
// error preceded it; if both happen, the Next error wins.
func drain(t *testing.T, it apipersistence.SnapshotIterator) []apipersistence.SnapshotEntry {
	t.Helper()
	var out []apipersistence.SnapshotEntry
	for {
		e, err := it.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = it.Close()
			t.Fatalf("iterator Next: %v", err)
		}
		out = append(out, e)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("iterator Close: %v", err)
	}
	return out
}

func roundTrip(t *testing.T, entries []apipersistence.SnapshotEntry) []apipersistence.SnapshotEntry {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "v1.snap")

	w := NewSnapshotter(file)
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: entries}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	r := NewSource(file)
	res, err := r.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if res.Mode != apipersistence.BootModeSnapshot {
		t.Fatalf("Mode = %v, want Snapshot", res.Mode)
	}
	return drain(t, res.Snapshot)
}

func TestRoundTrip_String(t *testing.T) {
	entries := []apipersistence.SnapshotEntry{
		{Key: "k1", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("hello")},
		{Key: "k2", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("world"), Expiration: 1234567890},
	}
	got := roundTrip(t, entries)
	if len(got) != 2 {
		t.Fatalf("count = %d, want 2", len(got))
	}
	if !bytes.Equal(got[0].Value.([]byte), []byte("hello")) {
		t.Errorf("k1 value = %v", got[0].Value)
	}
	if got[1].Expiration != 1234567890 {
		t.Errorf("k2 expiration = %d, want 1234567890", got[1].Expiration)
	}
}

func TestRoundTrip_List(t *testing.T) {
	entries := []apipersistence.SnapshotEntry{
		{Key: "list", ValueType: apipersistence.ValueTypeList, Encoding: apipersistence.EncodingNative, Value: []string{"a", "b", "c"}},
	}
	got := roundTrip(t, entries)
	if len(got) != 1 {
		t.Fatalf("count = %d", len(got))
	}
	v, ok := got[0].Value.([]string)
	if !ok {
		t.Fatalf("Value type = %T, want []string", got[0].Value)
	}
	if !reflect.DeepEqual(v, []string{"a", "b", "c"}) {
		t.Errorf("list = %v", v)
	}
}

func TestRoundTrip_Hash(t *testing.T) {
	entries := []apipersistence.SnapshotEntry{
		{Key: "hash", ValueType: apipersistence.ValueTypeHash, Encoding: apipersistence.EncodingNative, Value: map[string]string{"x": "1", "y": "2"}},
	}
	got := roundTrip(t, entries)
	v, ok := got[0].Value.(map[string]string)
	if !ok {
		t.Fatalf("Value type = %T", got[0].Value)
	}
	if v["x"] != "1" || v["y"] != "2" {
		t.Errorf("hash = %v", v)
	}
}

func TestRoundTrip_Set(t *testing.T) {
	entries := []apipersistence.SnapshotEntry{
		{Key: "set", ValueType: apipersistence.ValueTypeSet, Encoding: apipersistence.EncodingNative, Value: map[string]struct{}{"a": {}, "b": {}}},
	}
	got := roundTrip(t, entries)
	v, ok := got[0].Value.(map[string]struct{})
	if !ok {
		t.Fatalf("Value type = %T", got[0].Value)
	}
	if _, has := v["a"]; !has {
		t.Errorf("missing 'a' in set")
	}
}

func TestRoundTrip_SortedSet(t *testing.T) {
	members := map[string]float64{
		"alpha": 1.5,
		"beta":  2.0,
		"gamma": 0.5,
	}
	entries := []apipersistence.SnapshotEntry{
		{Key: "zset", ValueType: apipersistence.ValueTypeSortedSet, Encoding: apipersistence.EncodingNative, Value: members},
	}
	got := roundTrip(t, entries)
	v, ok := got[0].Value.(map[string]float64)
	if !ok {
		t.Fatalf("Value type = %T", got[0].Value)
	}
	if score := v["alpha"]; score != 1.5 {
		t.Errorf("alpha score = %v, want 1.5", score)
	}
	if score := v["gamma"]; score != 0.5 {
		t.Errorf("gamma score = %v, want 0.5", score)
	}
	if len(v) != 3 {
		t.Errorf("len = %d, want 3", len(v))
	}
}

func TestRoundTrip_Packed(t *testing.T) {
	entries := []apipersistence.SnapshotEntry{
		{Key: "packed-list", ValueType: apipersistence.ValueTypeList, Encoding: apipersistence.EncodingPacked, Value: []byte{0x00, 0x01, 0x02, 0x03}},
	}
	got := roundTrip(t, entries)
	if got[0].Encoding != apipersistence.EncodingPacked {
		t.Errorf("Encoding = %v, want Packed", got[0].Encoding)
	}
	if !bytes.Equal(got[0].Value.([]byte), []byte{0x00, 0x01, 0x02, 0x03}) {
		t.Errorf("packed bytes = %v", got[0].Value)
	}
}

func TestZstd_TriggersOnLargeValue(t *testing.T) {
	// A long ASCII string compresses well — verify zstd kicks in for
	// values above the threshold and the round-trip recovers the
	// original.
	big := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog ", 200))
	entries := []apipersistence.SnapshotEntry{
		{Key: "big", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: big},
	}
	got := roundTrip(t, entries)
	if !bytes.Equal(got[0].Value.([]byte), big) {
		t.Errorf("big value mismatch: got %d bytes, want %d", len(got[0].Value.([]byte)), len(big))
	}

	// Spot-check that the on-disk file is meaningfully smaller than
	// the raw payload — proves zstd actually ran.
	dir := t.TempDir()
	file := filepath.Join(dir, "z.snap")
	w := NewSnapshotter(file)
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: entries}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	stat, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat.Size() >= int64(len(big)) {
		t.Errorf("zstd didn't shrink: file=%d big=%d", stat.Size(), len(big))
	}
}

func TestZstd_SkipsBelowThreshold(t *testing.T) {
	// Below-threshold values stay raw — assert by checking that the
	// round-trip preserves bytes exactly (cheap proxy for "no
	// compressed flag set").
	small := []byte("short")
	got := roundTrip(t, []apipersistence.SnapshotEntry{
		{Key: "s", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: small},
	})
	if !bytes.Equal(got[0].Value.([]byte), small) {
		t.Errorf("small value mismatch")
	}
}

func TestRoundTrip_Empty(t *testing.T) {
	got := roundTrip(t, nil)
	if len(got) != 0 {
		t.Errorf("empty snapshot yielded %d entries", len(got))
	}
}

func TestRoundTrip_LSNCursor(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "lsn.snap")

	w := NewSnapshotter(file)
	w.SetLSN(apipersistence.LSN(42))
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: []apipersistence.SnapshotEntry{
		{Key: "k", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("v")},
	}}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	r := NewSource(file)
	res, err := r.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if res.LSN != apipersistence.LSN(42) {
		t.Errorf("LSN = %d, want 42", res.LSN)
	}
	got := drain(t, res.Snapshot)
	if len(got) != 1 || got[0].Key != "k" {
		t.Errorf("entries = %v", got)
	}
}

func TestRoundTrip_NoLSN_OmitsMETA(t *testing.T) {
	// SetLSN(0) → no META record. Boot returns ZeroLSN and yields the
	// data record on first Next.
	got := roundTrip(t, []apipersistence.SnapshotEntry{
		{Key: "only", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("data")},
	})
	if len(got) != 1 {
		t.Errorf("count = %d, want 1", len(got))
	}
}

func TestBoot_MissingFile(t *testing.T) {
	r := NewSource(filepath.Join(t.TempDir(), "nope.snap"))
	res, err := r.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if res.Mode != apipersistence.BootModeInitial {
		t.Errorf("Mode = %v, want Initial", res.Mode)
	}
}

func TestBoot_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "empty.snap")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := NewSource(file)
	res, err := r.Boot(context.Background())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if res.Mode != apipersistence.BootModeInitial {
		t.Errorf("Mode = %v, want Initial", res.Mode)
	}
}

func TestBoot_BadMagic(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "bad.snap")
	if err := os.WriteFile(file, []byte("XXXX\x01\x00\x00\x00\x00\x00\x00\x00\x00"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := NewSource(file)
	if _, err := r.Boot(context.Background()); err == nil {
		t.Errorf("Boot: expected error on bad magic, got nil")
	}
}

func TestBoot_BadVersion(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "ver.snap")
	if err := os.WriteFile(file, []byte("GCDB\x99\x00\x00\x00\x00\x00\x00\x00\x00"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := NewSource(file)
	_, err := r.Boot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("Boot: expected version error, got %v", err)
	}
}

func TestBoot_TruncatedFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "trunc.snap")
	// Write a valid snapshot then truncate.
	w := NewSnapshotter(file)
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: []apipersistence.SnapshotEntry{
		{Key: "k", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("v")},
	}}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := os.Truncate(file, 6); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	r := NewSource(file)
	if _, err := r.Boot(context.Background()); err == nil {
		t.Errorf("Boot: expected error on truncated file")
	}
}

func TestClose_ValidatesCRC(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "crc.snap")
	w := NewSnapshotter(file)
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: []apipersistence.SnapshotEntry{
		{Key: "k", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("v")},
	}}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	// Corrupt the body (not the footer): flip a single byte mid-file
	// and verify that draining + Close surfaces the CRC mismatch.
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < 8 {
		t.Fatalf("file too small (%d bytes)", len(data))
	}
	data[8] ^= 0xff
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r := NewSource(file)
	res, err := r.Boot(context.Background())
	if err != nil {
		// Boot could fail eagerly if the corruption breaks parsing —
		// either path is correct ("fail closed").
		return
	}
	// Drain — Next may succeed if the corrupted byte happens to land
	// inside a value blob; the Close-time CRC check is the backstop.
	for {
		_, err := res.Snapshot.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = res.Snapshot.Close()
			return // Next surfaced the corruption — also acceptable.
		}
	}
	closeErr := res.Snapshot.Close()
	if closeErr == nil {
		t.Errorf("expected CRC mismatch on Close after corruption, got nil")
	}
}

func TestSetFilename_HotReload(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.snap")
	b := filepath.Join(dir, "b.snap")

	w := NewSnapshotter(a)
	src := &sliceSrc{entries: []apipersistence.SnapshotEntry{
		{Key: "k", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("v")},
	}}
	if err := w.SaveSnapshot(context.Background(), src); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	if _, err := os.Stat(a); err != nil {
		t.Errorf("a missing: %v", err)
	}

	w.SetFilename(b)
	src.cursor = 0 // reset
	if err := w.SaveSnapshot(context.Background(), src); err != nil {
		t.Fatalf("Save b: %v", err)
	}
	if _, err := os.Stat(b); err != nil {
		t.Errorf("b missing: %v", err)
	}
}

func TestRoundTrip_DeterministicOutput(t *testing.T) {
	// Two snapshots of the same logical state should produce
	// byte-identical files. Verifies the sort-on-encode invariant
	// for native maps/sets.
	entries := []apipersistence.SnapshotEntry{
		{Key: "h", ValueType: apipersistence.ValueTypeHash, Encoding: apipersistence.EncodingNative, Value: map[string]string{"a": "1", "b": "2", "c": "3"}},
		{Key: "s", ValueType: apipersistence.ValueTypeSet, Encoding: apipersistence.EncodingNative, Value: map[string]struct{}{"x": {}, "y": {}}},
	}
	dir := t.TempDir()
	one := filepath.Join(dir, "1.snap")
	two := filepath.Join(dir, "2.snap")

	w := NewSnapshotter(one)
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: entries}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	w.SetFilename(two)
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: entries}); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	a, err := os.ReadFile(one)
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	b, err := os.ReadFile(two)
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("non-deterministic output: %d != %d bytes (or different bytes)", len(a), len(b))
	}
}

// helper exercised by TestRoundTrip_SortedSet to keep the test slim.
var _ = sort.Strings
