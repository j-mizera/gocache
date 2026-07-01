package observability

import (
	"sync/atomic"

	apiobs "gocache/api/observability"
)

// RecordArena is a per-operation record chain that grows by linking chunks from
// a ChunkPool. Its single producer writes data first, then publishes the safe
// snapshot boundary through totalRecords.
type RecordArena struct {
	head         *RecordChunk
	tail         *RecordChunk
	totalRecords atomic.Uint64
	localCount   uint64
	tailBase     uint64
	pool         *ChunkPool
}

// NewRecordArena creates an arena with an initial 32-record chunk. If the pool
// cannot provide that chunk, Append returns false until a new arena is created.
func NewRecordArena(pool *ChunkPool) *RecordArena {
	arena := &RecordArena{pool: pool}
	chunk := pool.Get(0)
	if chunk == nil {
		return arena
	}
	arena.head = chunk
	arena.tail = chunk
	return arena
}

// Append stores record in the current tail chunk, publishing the new count only
// after the record data and chunk length are visible to snapshot readers.
func (a *RecordArena) Append(record apiobs.TelemetryRecord) bool {
	if a == nil || a.tail == nil || a.pool == nil {
		return false
	}

	indexInTail := a.localCount - a.tailBase
	tailCapacity := uint64(a.pool.ClassSize(int(a.tail.classIdx)))
	if indexInTail >= tailCapacity {
		if !a.grow(tailCapacity) {
			return false
		}
		indexInTail = 0
	}

	a.tail.records[indexInTail] = record
	a.tail.len = indexInTail + 1
	a.localCount++
	a.totalRecords.Store(a.localCount)
	return true
}

// SnapshotCount returns the published safe-read boundary for concurrent
// snapshot readers.
func (a *RecordArena) SnapshotCount() uint64 {
	if a == nil {
		return 0
	}
	return a.totalRecords.Load()
}

// SnapshotRead copies up to count records from the arena chain using the
// atomic totalRecords boundary. It is safe for concurrent callers reading an
// active arena — it never touches the non-atomic localCount field.
func (a *RecordArena) SnapshotRead() []apiobs.TelemetryRecord {
	if a == nil {
		return nil
	}
	count := a.totalRecords.Load()
	if count == 0 {
		return make([]apiobs.TelemetryRecord, 0)
	}
	records := make([]apiobs.TelemetryRecord, 0, count)
	remaining := count
	chunk := a.head
	for remaining > 0 && chunk != nil {
		chunkCapacity := uint64(len(chunk.records))
		n := chunkCapacity
		if n > remaining {
			n = remaining
		}
		records = append(records, chunk.records[:n]...)
		remaining -= n
		if remaining > 0 {
			chunk = chunk.next.Load()
		}
	}
	return records
}

// Drain copies all records from the chain into a contiguous slice. It is called
// only after terminal state transfer gives the drain worker exclusive ownership.
func (a *RecordArena) Drain() []apiobs.TelemetryRecord {
	if a == nil {
		return nil
	}

	if a.localCount == 0 {
		return make([]apiobs.TelemetryRecord, 0)
	}

	remaining := a.localCount
	records := make([]apiobs.TelemetryRecord, 0, a.localCount)
	for chunk := a.head; chunk != nil && remaining > 0; chunk = chunk.next.Load() {
		chunkCapacity := uint64(len(chunk.records))
		chunkRecords := chunkCapacity
		if chunk.len < chunkRecords {
			chunkRecords = chunk.len
		}
		if chunkRecords > remaining {
			chunkRecords = remaining
		}
		records = append(records, chunk.records[:chunkRecords]...)
		remaining -= chunkRecords
	}
	return records
}

// DrainInto copies all records into the provided buffer, growing it if needed.
// After warmup the buffer retains its max-ever-seen capacity, eliminating
// per-operation allocation on the drain path. The returned slice aliases buf's
// backing array. Called only after terminal state transfer (exclusive ownership).
func (a *RecordArena) DrainInto(buf []apiobs.TelemetryRecord) []apiobs.TelemetryRecord {
	if a == nil {
		return buf[:0]
	}
	count := a.localCount
	if count == 0 {
		return buf[:0]
	}
	if cap(buf) < int(count) {
		buf = make([]apiobs.TelemetryRecord, count)
	} else {
		buf = buf[:count]
	}
	idx := 0
	for chunk := a.head; chunk != nil && idx < int(count); chunk = chunk.next.Load() {
		chunkLen := chunk.len
		if uint64(idx)+chunkLen > count {
			chunkLen = count - uint64(idx)
		}
		copy(buf[idx:], chunk.records[:chunkLen])
		idx += int(chunkLen)
	}
	return buf
}

// Reset returns every chunk in the arena chain to pool and clears arena state.
func (a *RecordArena) Reset(pool *ChunkPool) {
	if a == nil {
		return
	}
	returnPool := pool
	if returnPool == nil {
		returnPool = a.pool
	}

	for chunk := a.head; chunk != nil; {
		nextChunk := chunk.next.Load()
		returnPool.Put(chunk)
		chunk = nextChunk
	}

	a.head = nil
	a.tail = nil
	a.totalRecords.Store(0)
	a.localCount = 0
	a.tailBase = 0
	a.pool = nil
}

func (a *RecordArena) grow(tailCapacity uint64) bool {
	nextClass := int(a.tail.classIdx) + 1
	if nextClass >= numChunkClasses {
		nextClass = numChunkClasses - 1
	}

	nextChunk := a.pool.Get(nextClass)
	if nextChunk == nil {
		return false
	}
	a.tail.next.Store(nextChunk)
	a.tail = nextChunk
	a.tailBase += tailCapacity
	return true
}
