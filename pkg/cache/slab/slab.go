package slab

// slab is one pre-allocated byte region sliced into fixed-size entries. Once
// constructed, data is never re-sliced or reallocated — handing out zero-copy
// []byte views of sub-regions is safe because the backing array is stable.
type slab struct {
	classID        uint8
	classSize      uint32
	entriesPerSlab uint32

	data []byte // len = classSize * entriesPerSlab; never grows

	// valueLen stores the logical length of the bytes written into each
	// entry. Separate from the fixed classSize capacity; readers return
	// data[start:start+valueLen].
	valueLen []uint32

	// meta stores LRU pointers + LastAccessNs per entry. A flat array with
	// no pointer fields — GC-opaque. Updated under the cache write lock.
	meta []SlotMeta

	// freeList is a LIFO stack of free entry indices. Sized to
	// entriesPerSlab; len shrinks as entries are allocated.
	freeList []uint32
}

func newSlab(classID uint8, classSize uint32, entriesPerSlab uint32) *slab {
	s := &slab{
		classID:        classID,
		classSize:      classSize,
		entriesPerSlab: entriesPerSlab,
		data:           make([]byte, classSize*entriesPerSlab),
		valueLen:       make([]uint32, entriesPerSlab),
		meta:           make([]SlotMeta, entriesPerSlab),
		freeList:       make([]uint32, entriesPerSlab),
	}
	// Initial free list: all entries free, popped from the end.
	for i := uint32(0); i < entriesPerSlab; i++ {
		s.freeList[i] = entriesPerSlab - 1 - i
	}
	return s
}

// inUse reports how many entries are currently allocated.
func (s *slab) inUse() uint32 {
	return s.entriesPerSlab - uint32(len(s.freeList))
}

// hasFree reports whether at least one entry slot is available.
func (s *slab) hasFree() bool {
	return len(s.freeList) > 0
}

// allocEntry pops a free entry index. Caller must check hasFree() first.
func (s *slab) allocEntry() uint32 {
	n := len(s.freeList)
	idx := s.freeList[n-1]
	s.freeList = s.freeList[:n-1]
	return idx
}

// freeEntry returns an entry index to the free pool.
func (s *slab) freeEntry(idx uint32) {
	s.valueLen[idx] = 0
	s.meta[idx] = SlotMeta{}
	s.freeList = append(s.freeList, idx)
}

// entryBytes returns a zero-copy slice of the entry's capacity region. The
// returned slice aliases the slab's backing array; the caller must not retain
// it past the allocation's lifetime.
func (s *slab) entryCap(idx uint32) []byte {
	start := uint32(idx) * s.classSize
	return s.data[start : start+s.classSize]
}

// entryValue returns a zero-copy slice of len = valueLen[idx]. Use for reads.
func (s *slab) entryValue(idx uint32) []byte {
	start := uint32(idx) * s.classSize
	return s.data[start : start+s.valueLen[idx]]
}

// write copies src into the entry slot and records its length. The slot must
// be large enough (enforced at the class level by size-based allocation).
func (s *slab) write(idx uint32, src []byte) {
	start := uint32(idx) * s.classSize
	copy(s.data[start:start+s.classSize], src)
	s.valueLen[idx] = uint32(len(src))
}
