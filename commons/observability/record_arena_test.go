package observability

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	apiobs "gocache/api/observability"
)

func TestRecordArena_SmallOperation(t *testing.T) {
	pool := NewChunkPool(8)
	arena := NewRecordArena(pool)

	for i := 0; i < 4; i++ {
		if ok := arena.Append(recordArenaTestRecord(i)); !ok {
			t.Fatalf("Append(%d) = false, want true", i)
		}
	}

	assertRecordArenaDrain(t, arena, 4)
}

func TestRecordArena_Growth_50(t *testing.T) {
	pool := NewChunkPool(8)
	arena := NewRecordArena(pool)

	for i := 0; i < 50; i++ {
		if ok := arena.Append(recordArenaTestRecord(i)); !ok {
			t.Fatalf("Append(%d) = false, want true", i)
		}
	}

	assertRecordArenaDrain(t, arena, 50)
}

func TestRecordArena_Growth_200(t *testing.T) {
	pool := NewChunkPool(8)
	arena := NewRecordArena(pool)

	for i := 0; i < 200; i++ {
		if ok := arena.Append(recordArenaTestRecord(i)); !ok {
			t.Fatalf("Append(%d) = false, want true", i)
		}
	}

	assertRecordArenaDrain(t, arena, 200)
}

func TestRecordArena_SnapshotCount(t *testing.T) {
	pool := NewChunkPool(8)
	arena := NewRecordArena(pool)

	for i := 0; i < 10; i++ {
		if got, want := arena.SnapshotCount(), uint64(i); got != want {
			t.Fatalf("SnapshotCount before Append(%d) = %d, want %d", i, got, want)
		}
		if ok := arena.Append(recordArenaTestRecord(i)); !ok {
			t.Fatalf("Append(%d) = false, want true", i)
		}
		if got, want := arena.SnapshotCount(), uint64(i+1); got != want {
			t.Fatalf("SnapshotCount after Append(%d) = %d, want %d", i, got, want)
		}
		if got, written := arena.SnapshotCount(), uint64(i+1); got > written {
			t.Fatalf("SnapshotCount after Append(%d) = %d, exceeds written records %d", i, got, written)
		}
	}
}

func TestRecordArena_Drain_Empty(t *testing.T) {
	pool := NewChunkPool(8)
	arena := NewRecordArena(pool)

	records := arena.Drain()
	if records == nil {
		t.Fatal("Drain() = nil, want empty non-nil slice")
	}
	if got, want := len(records), 0; got != want {
		t.Fatalf("len(Drain()) = %d, want %d", got, want)
	}
}

func TestRecordArena_Reset(t *testing.T) {
	pool := NewChunkPool(8)
	arena := NewRecordArena(pool)

	for i := 0; i < 50; i++ {
		if ok := arena.Append(recordArenaTestRecord(i)); !ok {
			t.Fatalf("Append(%d) = false, want true", i)
		}
	}

	arena.Reset(pool)
	if got, want := arena.SnapshotCount(), uint64(0); got != want {
		t.Fatalf("SnapshotCount after Reset = %d, want %d", got, want)
	}
	resetRecords := arena.Drain()
	if resetRecords == nil {
		t.Fatal("Drain after Reset = nil, want empty non-nil slice")
	}
	if got, want := len(resetRecords), 0; got != want {
		t.Fatalf("len(Drain after Reset) = %d, want %d", got, want)
	}
	if ok := arena.Append(recordArenaTestRecord(99)); ok {
		t.Fatal("Append after Reset = true, want false until a new arena is created")
	}

	stats := pool.Stats()
	if got, want := stats.ClassCounts, ([numChunkClasses]int64{1, 1, 0}); got != want {
		t.Fatalf("pool.Stats().ClassCounts after Reset = %v, want %v", got, want)
	}
}

func TestRecordArena_PoolExhaustion(t *testing.T) {
	pool := NewChunkPool(1)
	reservedClass1 := pool.Get(1)
	if reservedClass1 == nil {
		t.Fatal("pool.Get(1) = nil, want reserved class-1 chunk")
	}
	t.Cleanup(func() { pool.Put(reservedClass1) })
	arena := NewRecordArena(pool)

	for i := 0; i < 32; i++ {
		if ok := arena.Append(recordArenaTestRecord(i)); !ok {
			t.Fatalf("Append(%d) = false, want true before class-0 chunk is full", i)
		}
	}
	if got, want := arena.SnapshotCount(), uint64(32); got != want {
		t.Fatalf("SnapshotCount before exhausted growth = %d, want %d", got, want)
	}

	if ok := arena.Append(recordArenaTestRecord(32)); ok {
		t.Fatal("Append(32) = true, want false when class-1 chunk is exhausted")
	}
	if got, want := arena.SnapshotCount(), uint64(32); got != want {
		t.Fatalf("SnapshotCount after failed Append = %d, want %d", got, want)
	}
	assertRecordArenaDrain(t, arena, 32)
}

func TestRecordArena_ConcurrentSnapshotRead(t *testing.T) {
	pool := NewChunkPool(128)
	arena := NewRecordArena(pool)
	const totalRecords = 10_000

	var scheduledRecords atomic.Uint64
	var writerDone atomic.Bool
	var maxObserved atomic.Uint64
	errCh := make(chan error, 1)
	start := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer writerDone.Store(true)
		<-start
		for i := 0; i < totalRecords; i++ {
			scheduledRecords.Store(uint64(i + 1))
			if ok := arena.Append(recordArenaTestRecord(i)); !ok {
				select {
				case errCh <- fmt.Errorf("Append(%d) = false, want true", i):
				default:
				}
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		<-start
		for !writerDone.Load() {
			count := arena.SnapshotCount()
			for {
				currentMax := maxObserved.Load()
				if count <= currentMax || maxObserved.CompareAndSwap(currentMax, count) {
					break
				}
			}
			if scheduled := scheduledRecords.Load(); count > scheduled {
				select {
				case errCh <- fmt.Errorf("SnapshotCount() = %d, exceeds scheduled records %d", count, scheduled):
				default:
				}
				return
			}
		}
	}()

	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, want := arena.SnapshotCount(), uint64(totalRecords); got != want {
		t.Fatalf("final SnapshotCount() = %d, want %d", got, want)
	}
	if got := maxObserved.Load(); got > totalRecords {
		t.Fatalf("max observed SnapshotCount() = %d, want <= %d", got, totalRecords)
	}
}

func recordArenaTestRecord(index int) apiobs.TelemetryRecord {
	return apiobs.NewTelemetryRecord(apiobs.TelemetryRecordOperationStart, apiobs.InternalOperationIdentity(index))
}

func assertRecordArenaDrain(t *testing.T, arena *RecordArena, wantCount int) {
	t.Helper()

	records := arena.Drain()
	if records == nil {
		t.Fatal("Drain() = nil, want non-nil slice")
	}
	if got := len(records); got != wantCount {
		t.Fatalf("len(Drain()) = %d, want %d", got, wantCount)
	}
	for i, record := range records {
		if got, want := record.Kind, apiobs.TelemetryRecordOperationStart; got != want {
			t.Fatalf("records[%d].Kind = %v, want %v", i, got, want)
		}
		if got, want := record.Operation, apiobs.InternalOperationIdentity(i); got != want {
			t.Fatalf("records[%d].Operation = %d, want %d", i, got, want)
		}
	}
}
