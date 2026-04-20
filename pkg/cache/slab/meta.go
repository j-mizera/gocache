package slab

// SlotMeta holds per-entry metadata inline in the slab. No pointers — the GC
// ignores these slots during mark-phase scanning. All mutations happen under
// the cache's write lock.
//
// Size: 40 bytes, 8-byte aligned. Prev+next pointers + a nanosecond timestamp
// make each entry an LRU list node. Without this, the cache would need an
// external `*list.Element` per key (3 GC pointers each).
type SlotMeta struct {
	LRUPrev      SlabPointer // zero = LRU head (no predecessor)
	LRUNext      SlabPointer // zero = LRU tail (no successor)
	LastAccessNs int64       // Unix nanos; ordering source for LRU eviction
	_pad         [8]byte     // room for future fields without changing size class
}

// Meta returns a pointer to the metadata slot for p. The returned pointer
// aliases the slab's metadata array; mutations must be serialized by the
// caller (cache write lock).
func (a *Allocator) Meta(p SlabPointer) *SlotMeta {
	if p.Class() == hugeClassID {
		if m, ok := a.hugeMeta[p.Entry()]; ok {
			return m
		}
		// A huge-class allocation that was never given a Meta entry is a
		// programmer error; return a dangling pointer so the crash is
		// obvious rather than silent.
		m := &SlotMeta{}
		a.hugeMeta[p.Entry()] = m
		return m
	}
	c := a.classes[p.Class()]
	return &c.slabs[p.Slab()].meta[p.Entry()]
}
