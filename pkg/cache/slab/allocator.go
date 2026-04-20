package slab

import (
	"fmt"
	"math/bits"
)

// Class configuration. Regular classes are power-of-two bytes from 64 B up
// to 65536 B (11 classes, IDs 0..10). Values larger than 65536 go to the
// huge class (ID 255) which allocates one dedicated slab per entry.
const (
	minClassSize uint32 = 64
	maxClassSize uint32 = 65536
	numClasses          = 11 // 64, 128, 256, ..., 65536
	hugeClassID  uint8  = 255

	// targetSlabBytes is the nominal per-slab allocation. Each class sizes
	// its entriesPerSlab to approach this budget. 1 MiB keeps the freeList
	// bookkeeping cheap and the GC-visible surface small.
	targetSlabBytes uint32 = 1 << 20
)

// Allocator hands out SlabPointers. It is not internally synchronized; the
// cache's engine-level write lock serializes all mutation. Readers holding
// the cache RLock may call Read safely because slab data regions are
// allocated once and never moved.
type Allocator struct {
	classes []*slabClass

	// huge stores one entry per key for values exceeding maxClassSize.
	// hugeNext is the next assignable id; freed ids are pushed onto
	// hugeFree for reuse.
	huge     map[uint32][]byte
	hugeNext uint32
	hugeFree []uint32

	// Stats. Updated on every Alloc/Free.
	totalAllocs uint64
	totalFrees  uint64
}

// NewAllocator constructs a slab Allocator with the standard 11 regular
// classes plus the huge class for oversize values. Classes start empty; the
// first Alloc in a class creates its first slab.
//
// To keep SlabPointer(0) reserved as NilPointer, class 0's slab 0 entry 0 is
// pre-allocated and leaked at startup — a 64-byte sunk cost.
func NewAllocator() *Allocator {
	a := &Allocator{
		classes: make([]*slabClass, numClasses),
		huge:    make(map[uint32][]byte),
	}
	for i := 0; i < numClasses; i++ {
		classSize := minClassSize << i
		entries := targetSlabBytes / classSize
		if entries == 0 {
			entries = 1
		}
		a.classes[i] = newSlabClass(uint8(i), classSize, entries)
	}
	// Burn slot 0 of class 0 so NilPointer stays distinct from valid
	// pointers. The returned pointer is class=0 slab=0 entry=0 = 0.
	_ = a.classes[0].alloc()
	return a
}

// classIndexFor picks the smallest regular class whose entries can hold
// `size` bytes. Returns (index, true) for the regular path and (0, false) if
// the value must go to the huge path.
func classIndexFor(size uint32) (int, bool) {
	if size > maxClassSize {
		return 0, false
	}
	if size <= minClassSize {
		return 0, true
	}
	// Round up to the next power of two ≥ size, then divide by minClassSize.
	// bits.Len32(x-1) gives ceil(log2(x)) for x>0.
	needed := uint32(1) << uint32(bits.Len32(size-1))
	idx := bits.TrailingZeros32(needed / minClassSize)
	return idx, true
}

// Alloc reserves an entry slot sized to hold exactly `size` bytes. The slot
// is uninitialized; call Write to populate it before Read.
func (a *Allocator) Alloc(size uint32) SlabPointer {
	a.totalAllocs++
	if idx, ok := classIndexFor(size); ok {
		return a.classes[idx].alloc()
	}
	return a.allocHuge(size)
}

// Free releases the entry identified by p. Double-free is caller's error and
// will corrupt the allocator's free list; we do not guard against it in the
// hot path.
func (a *Allocator) Free(p SlabPointer) {
	a.totalFrees++
	if p.Class() == hugeClassID {
		a.freeHuge(p)
		return
	}
	a.classes[p.Class()].free(p)
}

// Read returns a zero-copy slice of p's bytes. The returned slice aliases
// the slab or huge-map entry; callers must not retain it past Free.
func (a *Allocator) Read(p SlabPointer) []byte {
	if p.Class() == hugeClassID {
		return a.huge[p.Entry()]
	}
	return a.classes[p.Class()].read(p)
}

// Write copies src into p's slot. The caller must have sized p to hold len(src).
func (a *Allocator) Write(p SlabPointer, src []byte) {
	if p.Class() == hugeClassID {
		// Huge entries are exactly-sized; overwrite in place if lengths
		// match, otherwise grow/shrink by reallocating.
		buf := a.huge[p.Entry()]
		if len(buf) == len(src) {
			copy(buf, src)
			return
		}
		dst := make([]byte, len(src))
		copy(dst, src)
		a.huge[p.Entry()] = dst
		return
	}
	a.classes[p.Class()].write(p, src)
}

// Capacity returns the slot's byte capacity (classSize for regular classes,
// exact value length for huge).
func (a *Allocator) Capacity(p SlabPointer) uint32 {
	if p.Class() == hugeClassID {
		return uint32(len(a.huge[p.Entry()]))
	}
	return a.classes[p.Class()].classSize
}

// Size returns the current value length stored at p.
func (a *Allocator) Size(p SlabPointer) uint32 {
	return uint32(len(a.Read(p)))
}

// Huge path ------------------------------------------------------------------

func (a *Allocator) allocHuge(size uint32) SlabPointer {
	var id uint32
	if n := len(a.hugeFree); n > 0 {
		id = a.hugeFree[n-1]
		a.hugeFree = a.hugeFree[:n-1]
	} else {
		a.hugeNext++
		id = a.hugeNext
	}
	a.huge[id] = make([]byte, size)
	return packPointer(hugeClassID, 0, id)
}

func (a *Allocator) freeHuge(p SlabPointer) {
	id := p.Entry()
	delete(a.huge, id)
	a.hugeFree = append(a.hugeFree, id)
}

// Stats -----------------------------------------------------------------------

// Stats is a point-in-time snapshot of allocator accounting. Safe to expose
// via INFO or admin endpoints.
type Stats struct {
	TotalAllocs     uint64
	TotalFrees      uint64
	LiveEntries     uint64
	CapacityBytes   int64 // total bytes reserved by all slabs + huge buffers
	AllocatedBytes  int64 // bytes charged to in-use entries (regular = classSize * inUse)
	HugeCount       uint64
	HugeBytes       int64
	PerClassInUse   [numClasses]uint32
	PerClassSlabs   [numClasses]uint32
}

// Stats returns a point-in-time snapshot.
func (a *Allocator) Stats() Stats {
	var s Stats
	s.TotalAllocs = a.totalAllocs
	s.TotalFrees = a.totalFrees
	for i, c := range a.classes {
		inUse := c.inUse()
		s.PerClassInUse[i] = inUse
		s.PerClassSlabs[i] = uint32(len(c.slabs))
		s.CapacityBytes += c.capacityBytes()
		s.AllocatedBytes += c.allocatedBytes()
		s.LiveEntries += uint64(inUse)
	}
	s.HugeCount = uint64(len(a.huge))
	for _, buf := range a.huge {
		s.HugeBytes += int64(len(buf))
	}
	s.CapacityBytes += s.HugeBytes
	s.AllocatedBytes += s.HugeBytes
	s.LiveEntries += s.HugeCount
	return s
}

// String renders Stats in a form useful for INFO output / debug logging.
func (s Stats) String() string {
	return fmt.Sprintf(
		"slab: allocs=%d frees=%d live=%d capacity=%d allocated=%d huge=%d (%d bytes)",
		s.TotalAllocs, s.TotalFrees, s.LiveEntries,
		s.CapacityBytes, s.AllocatedBytes, s.HugeCount, s.HugeBytes,
	)
}
