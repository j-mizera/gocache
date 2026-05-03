package v1snap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apipersistence "gocache/api/persistence"
)

// makeEntries fabricates a workload-shape mix of snapshot entries:
//   - 60% small string keys (compress threshold not hit)
//   - 30% medium strings (~1 KB; zstd kicks in)
//   - 10% small lists (5 items each)
//
// Reflects the dominant shape of the cache write path under typical
// Redis-style workloads: lots of GET/SET, some structured data.
func makeEntries(n int) []apipersistence.SnapshotEntry {
	entries := make([]apipersistence.SnapshotEntry, 0, n)
	for i := 0; i < n; i++ {
		switch i % 10 {
		case 0:
			entries = append(entries, apipersistence.SnapshotEntry{
				Key:       fmt.Sprintf("list:%d", i),
				ValueType: apipersistence.ValueTypeList,
				Encoding:  apipersistence.EncodingNative,
				Value:     []string{"a", "b", "c", "d", "e"},
			})
		case 1, 2, 3:
			entries = append(entries, apipersistence.SnapshotEntry{
				Key:       fmt.Sprintf("med:%d", i),
				ValueType: apipersistence.ValueTypeBytes,
				Encoding:  apipersistence.EncodingNative,
				Value:     []byte(strings.Repeat("compressible-payload-", 50)),
			})
		default:
			entries = append(entries, apipersistence.SnapshotEntry{
				Key:       fmt.Sprintf("k:%d", i),
				ValueType: apipersistence.ValueTypeBytes,
				Encoding:  apipersistence.EncodingNative,
				Value:     []byte(fmt.Sprintf("v:%d", i)),
			})
		}
	}
	return entries
}

func BenchmarkV1_SaveSnapshot_10K(b *testing.B) {
	entries := makeEntries(10_000)
	dir := b.TempDir()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		file := filepath.Join(dir, fmt.Sprintf("s%d.snap", i))
		w := NewSnapshotter(file)
		if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: entries}); err != nil {
			b.Fatalf("SaveSnapshot: %v", err)
		}
		_ = os.Remove(file)
	}
}

func BenchmarkV1_BootDrain_10K(b *testing.B) {
	entries := makeEntries(10_000)
	dir := b.TempDir()
	file := filepath.Join(dir, "boot.snap")

	w := NewSnapshotter(file)
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: entries}); err != nil {
		b.Fatalf("SaveSnapshot: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := NewSource(file)
		res, err := r.Boot(context.Background())
		if err != nil {
			b.Fatalf("Boot: %v", err)
		}
		count := 0
		for {
			_, err := res.Snapshot.Next(context.Background())
			if err != nil {
				break
			}
			count++
		}
		if err := res.Snapshot.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		if count != 10_000 {
			b.Fatalf("count = %d, want 10000", count)
		}
	}
}

func BenchmarkV1_FileSize_10K(b *testing.B) {
	// Not a perf bench, but useful as a single-shot size measurement —
	// run with -bench=BenchmarkV1_FileSize -benchtime=1x to get one
	// data point including ReportMetric for bytes.
	entries := makeEntries(10_000)
	dir := b.TempDir()
	file := filepath.Join(dir, "size.snap")
	w := NewSnapshotter(file)
	if err := w.SaveSnapshot(context.Background(), &sliceSrc{entries: entries}); err != nil {
		b.Fatalf("SaveSnapshot: %v", err)
	}
	stat, err := os.Stat(file)
	if err != nil {
		b.Fatalf("Stat: %v", err)
	}
	b.ReportMetric(float64(stat.Size()), "bytes/file")
	b.ReportMetric(float64(stat.Size())/10_000.0, "bytes/entry")
}
