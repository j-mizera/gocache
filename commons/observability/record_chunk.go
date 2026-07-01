package observability

import (
	"sync"
	"sync/atomic"

	apiobs "gocache/api/observability"
)

const (
	numChunkClasses        = 3
	defaultChunkClassLimit = 512
)

var chunkClassSizes = [numChunkClasses]int{32, 64, 128}

// RecordChunk is a fixed-capacity telemetry record chunk allocated for one size
// class. The records backing array contains pointer-free TelemetryRecord values,
// so only the slice header is scanned by GC; record contents are not.
//
// The len field is non-atomic because an active chunk is owned by a single
// producer goroutine. Publication belongs to RecordArena.totalRecords.
//
// The next field atomically links an arena chain while active so snapshot
// readers can follow published chunks while the producer grows the chain.
// ChunkPool.Get clears next before returning a chunk to arena ownership.
type RecordChunk struct {
	records  []apiobs.TelemetryRecord
	len      uint64
	classIdx uint8
	next     atomic.Pointer[RecordChunk]
}

// ChunkPoolStats reports the number of retained free chunks per class.
type ChunkPoolStats struct {
	ClassCounts [numChunkClasses]int64
}

// ChunkPool is a three-class mutex-protected LIFO free list for RecordChunks.
// It does not use sync.Pool because sync.Pool may drop retained chunks on GC.
type ChunkPool struct {
	classes [numChunkClasses]chunkClass
}

type chunkClass struct {
	mu        sync.Mutex
	free      []*RecordChunk
	allocated atomic.Int64
	maxChunks int64
}

// NewChunkPool creates a telemetry chunk pool. Non-positive limits use the
// default per-class cap.
func NewChunkPool(maxChunksPerClass int64) *ChunkPool {
	classLimit := maxChunksPerClass
	if classLimit <= 0 {
		classLimit = defaultChunkClassLimit
	}

	pool := &ChunkPool{}
	for classIndex := range pool.classes {
		pool.classes[classIndex].maxChunks = classLimit
	}
	return pool
}

// Get returns a free chunk for classIndex, allocating on demand until the class
// cap is reached. It returns nil on invalid classIndex or cap exhaustion.
func (p *ChunkPool) Get(classIndex int) *RecordChunk {
	if p == nil || !validChunkClass(classIndex) {
		return nil
	}

	chunkClassRef := &p.classes[classIndex]
	if chunk := chunkClassRef.pop(); chunk != nil {
		return chunk
	}

	if !chunkClassRef.reserveAllocation() {
		return nil
	}

	return newRecordChunk(classIndex)
}

// Put returns chunk to its class free list. Full pools discard the chunk and
// release its allocation budget so a later Get may allocate again.
func (p *ChunkPool) Put(chunk *RecordChunk) {
	if p == nil || chunk == nil {
		return
	}

	classIndex := int(chunk.classIdx)
	if !validChunkClass(classIndex) {
		return
	}

	// Do not clear records here: active writers overwrite before publishing.
	chunk.len = 0
	chunk.next.Store(nil)

	chunkClassRef := &p.classes[classIndex]
	if !chunkClassRef.push(chunk) {
		chunkClassRef.releaseAllocation()
	}
}

// ClassSize returns the logical chunk capacity for classIndex, or zero when the
// class is invalid.
func (p *ChunkPool) ClassSize(classIndex int) int {
	if !validChunkClass(classIndex) {
		return 0
	}
	return chunkClassSizes[classIndex]
}

// Stats returns per-class retained free chunk counts.
func (p *ChunkPool) Stats() ChunkPoolStats {
	var stats ChunkPoolStats
	if p == nil {
		return stats
	}

	for classIndex := range p.classes {
		chunkClassRef := &p.classes[classIndex]
		chunkClassRef.mu.Lock()
		stats.ClassCounts[classIndex] = int64(len(chunkClassRef.free))
		chunkClassRef.mu.Unlock()
	}
	return stats
}

func newRecordChunk(classIndex int) *RecordChunk {
	return &RecordChunk{
		records:  make([]apiobs.TelemetryRecord, chunkClassSizes[classIndex]),
		classIdx: uint8(classIndex),
	}
}

func validChunkClass(classIndex int) bool {
	return classIndex >= 0 && classIndex < numChunkClasses
}

func (c *chunkClass) pop() *RecordChunk {
	c.mu.Lock()
	defer c.mu.Unlock()

	chunkCount := len(c.free)
	if chunkCount == 0 {
		return nil
	}

	chunk := c.free[chunkCount-1]
	c.free[chunkCount-1] = nil
	c.free = c.free[:chunkCount-1]
	chunk.next.Store(nil)
	return chunk
}

func (c *chunkClass) push(chunk *RecordChunk) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if int64(len(c.free)) >= c.maxChunks {
		return false
	}

	c.free = append(c.free, chunk)
	return true
}

func (c *chunkClass) reserveAllocation() bool {
	for {
		allocatedChunks := c.allocated.Load()
		if allocatedChunks >= c.maxChunks {
			return false
		}
		if c.allocated.CompareAndSwap(allocatedChunks, allocatedChunks+1) {
			return true
		}
	}
}

func (c *chunkClass) releaseAllocation() {
	c.allocated.Add(-1)
}
