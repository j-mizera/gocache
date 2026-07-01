package observability

import (
	"fmt"
	"sync"
	"testing"

	apiobs "gocache/api/observability"
)

// legacyFlatArrayWriter captures the pre-Phase-6 flat-array record storage for
// benchmark comparison. It mirrors the old segment.records[slot*rpo+count] write path.
type legacyFlatArrayWriter struct {
	records             []apiobs.TelemetryRecord
	recordsPerOperation int
}

func newLegacyFlatArrayWriter(segmentSize, recordsPerOperation int) *legacyFlatArrayWriter {
	return &legacyFlatArrayWriter{
		records:             make([]apiobs.TelemetryRecord, segmentSize*recordsPerOperation),
		recordsPerOperation: recordsPerOperation,
	}
}

func (w *legacyFlatArrayWriter) write(slot int, count int, record apiobs.TelemetryRecord) bool {
	if count >= w.recordsPerOperation {
		return false
	}
	w.records[slot*w.recordsPerOperation+count] = record
	return true
}

// BenchmarkArenaAppend measures arena Append throughput at various record counts.
// Target: zero allocations after pool warmup.
func BenchmarkArenaAppend(b *testing.B) {
	pool := NewChunkPool(0) // default cap
	arena := NewRecordArena(pool)
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !arena.Append(record) {
			b.Fatal("arena append failed — pool exhausted")
		}
		// Reset arena every 1000 records to avoid unbounded growth
		if i%1000 == 999 {
			arena.Reset(pool)
			arena = NewRecordArena(pool)
		}
	}
}

// BenchmarkLegacyFlatArrayWrite measures old flat-array write for comparison.
func BenchmarkLegacyFlatArrayWrite(b *testing.B) {
	writer := newLegacyFlatArrayWriter(1, 1024)
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writer.write(0, i%1024, record)
	}
}

// BenchmarkArenaDrain measures drain latency for various operation sizes.
func BenchmarkArenaDrain(b *testing.B) {
	sizes := []int{4, 20, 50, 200, 500}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("drain-%d", size), func(b *testing.B) {
			pool := NewChunkPool(0)
			record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)

			// Pre-fill arena
			arena := NewRecordArena(pool)
			for j := 0; j < size; j++ {
				arena.Append(record)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = arena.Drain()
			}
		})
	}
}

// BenchmarkArenaSnapshotRead measures concurrent-safe snapshot read.
func BenchmarkArenaSnapshotRead(b *testing.B) {
	sizes := []int{4, 20, 50, 200}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("snapshot-%d", size), func(b *testing.B) {
			pool := NewChunkPool(0)
			record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)

			arena := NewRecordArena(pool)
			for j := 0; j < size; j++ {
				arena.Append(record)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = arena.SnapshotRead()
			}
		})
	}
}

// BenchmarkArenaConcurrentStress measures throughput under concurrent access.
// Multiple goroutines append + snapshot-read simultaneously.
func BenchmarkArenaConcurrentStress(b *testing.B) {
	pool := NewChunkPool(0)
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		arena := NewRecordArena(pool)
		var wg sync.WaitGroup
		// Writer
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				arena.Append(record)
			}
		}()
		// Reader
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = arena.SnapshotRead()
			}
		}()
		wg.Wait()
		arena.Reset(pool)
	}
}

// BenchmarkFullSlotTrackerCycle measures the full StartOp→RecordTelemetry×N→FinishOp→Drain cycle.
// This is the end-to-end comparison with the old flat-array path.
func BenchmarkFullSlotTrackerCycle(b *testing.B) {
	sizes := []int{4, 20, 50, 200}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("cycle-%d", size), func(b *testing.B) {
			manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
				ShardCount:            1,
				MinSegmentsPerShard:   1,
				MaxSegmentsPerShard:   1,
				SegmentSize:           256,
				CompletedRingPerShard: 256,
			})
			record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				handle, ok := manager.StartOperation(apiobs.InternalOperationIdentity(i+1), apiobs.ParentRef{}, 0)
				if !ok {
					b.Fatal("start failed")
				}
				for j := 0; j < size; j++ {
					manager.RecordTelemetry(handle, record)
				}
				manager.FinishOperation(handle, SlotTerminalFinished)
				manager.DrainCompletedShard(0, func(CompletedOperation) {})
			}
		})
	}
}
