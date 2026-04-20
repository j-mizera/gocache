// Package slab implements a slab allocator backing the cache's packed byte
// entries. The goal is GC pressure reduction: instead of N distinct []byte
// allocations visible to the garbage collector, slab allocates a handful of
// large []byte regions and hands out integer offsets into them.
//
// SlabPointer is the identifier returned by Alloc. It packs (class, slab,
// entry) into a single uint64. Entries are a fixed size within a class; a
// class is a collection of slabs; a slab is one pre-allocated []byte region.
//
// SlabPointer(0) is the nil/unallocated sentinel. The allocator never returns
// it — class 0 reserves its first entry slot at startup.
package slab

// SlabPointer identifies one allocated entry. Layout:
//
//	[ 8 bits classID | 24 bits slabIndex | 32 bits entryIndex ]
//
// The uint64 is inert from the GC's perspective — no pointers, no scanning.
type SlabPointer uint64

// NilPointer is the zero value and means "no entry". Alloc never returns it.
const NilPointer SlabPointer = 0

const (
	classShift = 56
	slabShift  = 32
	slabMask   = 0x00FF_FFFF
)

func packPointer(classID uint8, slabIdx uint32, entryIdx uint32) SlabPointer {
	return SlabPointer(uint64(classID)<<classShift | uint64(slabIdx&slabMask)<<slabShift | uint64(entryIdx))
}

// Class returns the class ID component of p.
func (p SlabPointer) Class() uint8 { return uint8(p >> classShift) }

// Slab returns the slab-index component of p (within its class).
func (p SlabPointer) Slab() uint32 { return uint32(p>>slabShift) & slabMask }

// Entry returns the entry-index component of p (within its slab).
func (p SlabPointer) Entry() uint32 { return uint32(p) }

// IsNil reports whether p is the zero pointer.
func (p SlabPointer) IsNil() bool { return p == NilPointer }
