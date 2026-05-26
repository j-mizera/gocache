package slab

// slabClass manages all slabs of one entry size. Allocations from this class
// pop from a non-empty slab; on exhaustion a new slab is appended.
type slabClass struct {
	classID        uint8
	classSize      uint32
	entriesPerSlab uint32

	slabs []*slab

	// freeNonEmpty stores the indices (into slabs) of slabs that have at
	// least one free entry. Popping from the end + pushing to the end
	// keeps allocation O(1). A slab may appear at most once.
	freeNonEmpty []uint32
}

func newSlabClass(classID uint8, classSize uint32, entriesPerSlab uint32) *slabClass {
	return &slabClass{
		classID:        classID,
		classSize:      classSize,
		entriesPerSlab: entriesPerSlab,
	}
}

// alloc returns a pointer to a fresh entry in this class. On exhaustion it
// grows the class by one slab.
func (c *slabClass) alloc() SlabPointer {
	if len(c.freeNonEmpty) == 0 {
		c.growOneSlab()
	}
	slabIdx := c.freeNonEmpty[len(c.freeNonEmpty)-1]
	s := c.slabs[slabIdx]
	entry := s.allocEntry()
	if !s.hasFree() {
		// Slab is now full; drop it from the free list.
		c.freeNonEmpty = c.freeNonEmpty[:len(c.freeNonEmpty)-1]
	}
	return packPointer(c.classID, slabIdx, entry)
}

// free releases the entry identified by p back to this class's free pool. p
// must have been returned by this class's alloc.
func (c *slabClass) free(p SlabPointer) {
	slabIdx := p.Slab()
	s := c.slabs[slabIdx]
	wasFull := !s.hasFree()
	s.freeEntry(p.Entry())
	if wasFull {
		c.freeNonEmpty = append(c.freeNonEmpty, slabIdx)
	}
}

func (c *slabClass) growOneSlab() {
	if uint32(len(c.slabs)) >= (1 << 24) {
		panic("slab: exceeded 16M slabs")
	}
	s := newSlab(c.classID, c.classSize, c.entriesPerSlab)
	slabIdx := uint32(len(c.slabs))
	c.slabs = append(c.slabs, s)
	c.freeNonEmpty = append(c.freeNonEmpty, slabIdx)
}

// read returns a zero-copy view of the value bytes for p.
func (c *slabClass) read(p SlabPointer) []byte {
	return c.slabs[p.Slab()].entryValue(p.Entry())
}

// write copies src into p's slot. Caller has already chosen the class such
// that len(src) <= classSize.
func (c *slabClass) write(p SlabPointer, src []byte) {
	c.slabs[p.Slab()].write(p.Entry(), src)
}

// inUse returns how many entries across all slabs in this class are
// currently allocated.
func (c *slabClass) inUse() uint32 {
	var total uint32
	for _, s := range c.slabs {
		total += s.inUse()
	}
	return total
}

// capacityBytes returns the total bytes reserved by all slabs in the class.
func (c *slabClass) capacityBytes() int64 {
	return int64(len(c.slabs)) * int64(c.classSize) * int64(c.entriesPerSlab)
}

// allocatedBytes returns bytes accounted to in-use entries at class size.
// Fragmentation = capacityBytes - allocatedBytes.
func (c *slabClass) allocatedBytes() int64 {
	return int64(c.inUse()) * int64(c.classSize)
}
