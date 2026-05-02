package slab

// SlotMeta holds per-entry metadata inline in the slab. No pointers — the GC
// ignores these slots during mark-phase scanning. All mutations happen under
// the cache's write lock.
//
// Size: 40 bytes, 8-byte aligned (36 B of fields + 4 B of natural trailing
// pad). Prev+next pointers + a nanosecond timestamp make each entry an LRU
// list node. ValueType and Encoding live here so the forward key index can
// be a bare `map[string]SlabPointer` — no `*Entry` indirection.
// ExpirationNs replaces the Cache.ttl map — zero means "no TTL set".
// NativeSize caches the byte cost of an EncNative entry so chargedSize
// doesn't walk the underlying map/slice on every keySize lookup. Unused
// for EncPacked (size derives from the slab class). The cache package
// maps its own enums onto these bytes.
type SlotMeta struct {
	LRUPrev      SlabPointer // zero = LRU head (no predecessor)
	LRUNext      SlabPointer // zero = LRU tail (no successor)
	LastAccessNs int64       // Unix nanos; ordering source for LRU eviction
	ExpirationNs int64       // Unix nanos; zero = no TTL
	NativeSize   uint32      // cached byte cost for EncNative entries; zero for EncPacked
	ValueType    uint8       // cache.ValueType (ObjTypeBytes/List/Hash/Set/SortedSet)
	Encoding     uint8       // cache.Encoding (EncNative / EncPacked)
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
