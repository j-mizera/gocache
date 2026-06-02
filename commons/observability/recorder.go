package observability

import (
	"sync/atomic"

	apiobs "gocache/api/observability"
)

const cacheLineBytes = 64

type telemetryRecorder interface {
	RecordTelemetry(apiobs.TelemetryRecord) bool
	DroppedRecords() uint64
	drain(func(apiobs.TelemetryRecord)) int
}

// NewSingleProducerTelemetryRecorder returns a bounded, non-blocking recorder
// intended for one producer and one consumer. Shared shard trackers should use
// the multi-producer recorder hidden behind ShardedOperationTrackerManager.
func NewSingleProducerTelemetryRecorder(capacity int) apiobs.TelemetryRecorder {
	return newSingleProducerRecorder(capacity)
}

func newSingleProducerRecorder(capacity int) *singleProducerRecorder {
	return &singleProducerRecorder{ring: newRing(capacity)}
}

type singleProducerRecorder struct {
	ring *ring
}

func (r *singleProducerRecorder) RecordTelemetry(record apiobs.TelemetryRecord) bool {
	return r.ring.push(record)
}

func (r *singleProducerRecorder) DroppedRecords() uint64 {
	return r.ring.dropped()
}

func (r *singleProducerRecorder) drain(fn func(apiobs.TelemetryRecord)) int {
	return r.ring.drain(fn)
}

type ring struct {
	mask    uint64
	records []apiobs.TelemetryRecord
	_       [cacheLineBytes - 8]byte
	head    uint64
	_       [cacheLineBytes - 8]byte
	tail    uint64
	_       [cacheLineBytes - 8]byte
	drops   uint64
}

func newRing(capacity int) *ring {
	if capacity < 1 {
		capacity = 1
	}
	size := 1
	for size < capacity {
		size <<= 1
	}
	return &ring{mask: uint64(size - 1), records: make([]apiobs.TelemetryRecord, size)}
}

func (r *ring) push(record apiobs.TelemetryRecord) bool {
	tail := r.tail
	if tail-atomic.LoadUint64(&r.head) >= uint64(len(r.records)) {
		atomic.AddUint64(&r.drops, 1)
		return false
	}
	r.records[tail&r.mask] = record
	atomic.StoreUint64(&r.tail, tail+1)
	return true
}

func (r *ring) pop(record *apiobs.TelemetryRecord) bool {
	head := r.head
	if head == atomic.LoadUint64(&r.tail) {
		return false
	}
	*record = r.records[head&r.mask]
	atomic.StoreUint64(&r.head, head+1)
	return true
}

func (r *ring) drain(fn func(apiobs.TelemetryRecord)) int {
	var record apiobs.TelemetryRecord
	count := 0
	for r.pop(&record) {
		fn(record)
		count++
	}
	return count
}

func (r *ring) dropped() uint64 {
	return atomic.LoadUint64(&r.drops)
}

func newMultiProducerRecorder(capacity int) *multiProducerRecorder {
	return &multiProducerRecorder{ring: newMultiProducerRing(capacity)}
}

type multiProducerRecorder struct {
	ring *multiProducerRing
}

func (r *multiProducerRecorder) RecordTelemetry(record apiobs.TelemetryRecord) bool {
	return r.ring.push(record)
}

func (r *multiProducerRecorder) DroppedRecords() uint64 {
	return r.ring.dropped()
}

func (r *multiProducerRecorder) drain(fn func(apiobs.TelemetryRecord)) int {
	return r.ring.drain(fn)
}

type multiProducerRing struct {
	mask    uint64
	records []multiProducerRingSlot
	_       [cacheLineBytes - 8]byte
	head    uint64
	_       [cacheLineBytes - 8]byte
	tail    uint64
	_       [cacheLineBytes - 8]byte
	drops   uint64
}

type multiProducerRingSlot struct {
	sequence uint64
	record   apiobs.TelemetryRecord
}

func newMultiProducerRing(capacity int) *multiProducerRing {
	if capacity < 1 {
		capacity = 1
	}
	size := 1
	for size < capacity {
		size <<= 1
	}
	ring := &multiProducerRing{mask: uint64(size - 1), records: make([]multiProducerRingSlot, size)}
	for i := range ring.records {
		ring.records[i].sequence = uint64(i)
	}
	return ring
}

func (r *multiProducerRing) push(record apiobs.TelemetryRecord) bool {
	pos := atomic.LoadUint64(&r.tail)
	for {
		slot := &r.records[pos&r.mask]
		sequence := atomic.LoadUint64(&slot.sequence)
		delta := int64(sequence) - int64(pos)
		if delta == 0 {
			if atomic.CompareAndSwapUint64(&r.tail, pos, pos+1) {
				slot.record = record
				atomic.StoreUint64(&slot.sequence, pos+1)
				return true
			}
			pos = atomic.LoadUint64(&r.tail)
			continue
		}
		if delta < 0 {
			atomic.AddUint64(&r.drops, 1)
			return false
		}
		pos = atomic.LoadUint64(&r.tail)
	}
}

func (r *multiProducerRing) pop(record *apiobs.TelemetryRecord) bool {
	pos := atomic.LoadUint64(&r.head)
	for {
		slot := &r.records[pos&r.mask]
		sequence := atomic.LoadUint64(&slot.sequence)
		delta := int64(sequence) - int64(pos+1)
		if delta == 0 {
			if atomic.CompareAndSwapUint64(&r.head, pos, pos+1) {
				*record = slot.record
				atomic.StoreUint64(&slot.sequence, pos+r.mask+1)
				return true
			}
			pos = atomic.LoadUint64(&r.head)
			continue
		}
		if delta < 0 {
			return false
		}
		pos = atomic.LoadUint64(&r.head)
	}
}

func (r *multiProducerRing) drain(fn func(apiobs.TelemetryRecord)) int {
	var record apiobs.TelemetryRecord
	count := 0
	for r.pop(&record) {
		fn(record)
		count++
	}
	return count
}

func (r *multiProducerRing) dropped() uint64 {
	return atomic.LoadUint64(&r.drops)
}

func shardIndex(identity apiobs.InternalOperationIdentity, shardCount int) int {
	if shardCount <= 1 {
		return 0
	}
	return int(uint64(identity) % uint64(shardCount))
}
