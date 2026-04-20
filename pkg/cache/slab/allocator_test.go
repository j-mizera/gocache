package slab

import (
	"bytes"
	"testing"
)

func TestSlabPointer_PackUnpack(t *testing.T) {
	tests := []struct {
		name      string
		classID   uint8
		slabIdx   uint32
		entryIdx  uint32
	}{
		{"zero", 0, 0, 0},
		{"small", 1, 2, 3},
		{"mid", 10, 65535, 1_000_000},
		{"huge", 255, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := packPointer(tt.classID, tt.slabIdx, tt.entryIdx)
			if got := p.Class(); got != tt.classID {
				t.Errorf("Class() = %d, want %d", got, tt.classID)
			}
			if got := p.Slab(); got != tt.slabIdx {
				t.Errorf("Slab() = %d, want %d", got, tt.slabIdx)
			}
			if got := p.Entry(); got != tt.entryIdx {
				t.Errorf("Entry() = %d, want %d", got, tt.entryIdx)
			}
		})
	}
}

func TestSlabPointer_NilSentinel(t *testing.T) {
	if !NilPointer.IsNil() {
		t.Fatal("NilPointer should IsNil")
	}
	p := packPointer(1, 0, 0)
	if p.IsNil() {
		t.Fatal("non-zero pointer must not IsNil")
	}
}

func TestClassIndexFor(t *testing.T) {
	tests := []struct {
		size      uint32
		wantIdx   int
		wantOK    bool
		wantClass uint32
	}{
		{0, 0, true, 64},
		{1, 0, true, 64},
		{64, 0, true, 64},
		{65, 1, true, 128},
		{128, 1, true, 128},
		{129, 2, true, 256},
		{65535, 10, true, 65536},
		{65536, 10, true, 65536},
		{65537, 0, false, 0}, // huge
		{1 << 20, 0, false, 0},
	}
	for _, tt := range tests {
		idx, ok := classIndexFor(tt.size)
		if idx != tt.wantIdx || ok != tt.wantOK {
			t.Errorf("classIndexFor(%d) = (%d, %v), want (%d, %v)",
				tt.size, idx, ok, tt.wantIdx, tt.wantOK)
		}
		if ok {
			got := minClassSize << idx
			if got != tt.wantClass {
				t.Errorf("class size for %d = %d, want %d", tt.size, got, tt.wantClass)
			}
		}
	}
}

func TestAllocator_RoundTrip(t *testing.T) {
	a := NewAllocator()
	payload := []byte("hello, world")

	p := a.Alloc(uint32(len(payload)))
	if p.IsNil() {
		t.Fatal("alloc returned NilPointer")
	}
	if p.Class() != 0 {
		t.Fatalf("expected class 0 for 12 bytes, got %d", p.Class())
	}
	a.Write(p, payload)

	got := a.Read(p)
	if !bytes.Equal(got, payload) {
		t.Fatalf("read back %q, want %q", got, payload)
	}
	if a.Size(p) != uint32(len(payload)) {
		t.Fatalf("Size = %d, want %d", a.Size(p), len(payload))
	}
}

func TestAllocator_NilPointerReserved(t *testing.T) {
	a := NewAllocator()
	// First real alloc should not collide with NilPointer.
	p := a.Alloc(8)
	if p == NilPointer {
		t.Fatal("allocator returned NilPointer; class 0 slot 0 was not reserved")
	}
}

func TestAllocator_MultipleAllocsInSameClass(t *testing.T) {
	a := NewAllocator()
	pointers := make([]SlabPointer, 100)
	for i := 0; i < 100; i++ {
		pointers[i] = a.Alloc(32)
		payload := bytes.Repeat([]byte{byte(i)}, 32)
		a.Write(pointers[i], payload)
	}
	// All must be in class 0 (64-byte slots). All must be distinct.
	seen := make(map[SlabPointer]struct{}, 100)
	for i, p := range pointers {
		if p.Class() != 0 {
			t.Errorf("alloc %d: class = %d, want 0", i, p.Class())
		}
		if _, dup := seen[p]; dup {
			t.Errorf("duplicate pointer returned for alloc %d: %d", i, p)
		}
		seen[p] = struct{}{}
	}
	// Read back.
	for i, p := range pointers {
		got := a.Read(p)
		want := bytes.Repeat([]byte{byte(i)}, 32)
		if !bytes.Equal(got, want) {
			t.Errorf("alloc %d: read mismatch", i)
		}
	}
}

func TestAllocator_FreeAndReuse(t *testing.T) {
	a := NewAllocator()
	p1 := a.Alloc(10)
	a.Write(p1, []byte("first"))
	a.Free(p1)

	p2 := a.Alloc(10)
	if p2 != p1 {
		t.Errorf("expected slot reuse: p1=%d p2=%d", p1, p2)
	}
	a.Write(p2, []byte("second"))
	if got := string(a.Read(p2)); got != "second" {
		t.Errorf("read after reuse = %q, want %q", got, "second")
	}
}

func TestAllocator_GrowMultipleSlabs(t *testing.T) {
	a := NewAllocator()
	// Class 10 is 64 KiB × 16 entries per slab = 1 MiB slab. Allocate 40
	// entries to force 3 slabs.
	ps := make([]SlabPointer, 40)
	for i := range ps {
		ps[i] = a.Alloc(maxClassSize)
	}
	stats := a.Stats()
	if stats.PerClassSlabs[numClasses-1] < 3 {
		t.Errorf("expected ≥3 slabs in largest class, got %d", stats.PerClassSlabs[numClasses-1])
	}
	// Free all; inUse must drop to 0.
	for _, p := range ps {
		a.Free(p)
	}
	stats = a.Stats()
	if stats.PerClassInUse[numClasses-1] != 0 {
		t.Errorf("expected inUse=0 after freeing all, got %d", stats.PerClassInUse[numClasses-1])
	}
}

func TestAllocator_HugePath(t *testing.T) {
	a := NewAllocator()
	big := bytes.Repeat([]byte("x"), int(maxClassSize)+1024)

	p := a.Alloc(uint32(len(big)))
	if p.Class() != hugeClassID {
		t.Fatalf("expected huge class, got %d", p.Class())
	}
	a.Write(p, big)
	if got := a.Read(p); !bytes.Equal(got, big) {
		t.Errorf("huge round-trip mismatch (len got=%d want=%d)", len(got), len(big))
	}

	stats := a.Stats()
	if stats.HugeCount != 1 {
		t.Errorf("HugeCount = %d, want 1", stats.HugeCount)
	}
	if stats.HugeBytes != int64(len(big)) {
		t.Errorf("HugeBytes = %d, want %d", stats.HugeBytes, len(big))
	}

	a.Free(p)
	stats = a.Stats()
	if stats.HugeCount != 0 {
		t.Errorf("HugeCount after free = %d, want 0", stats.HugeCount)
	}
}

func TestAllocator_HugeReuse(t *testing.T) {
	a := NewAllocator()
	p1 := a.Alloc(maxClassSize + 1)
	a.Free(p1)
	p2 := a.Alloc(maxClassSize + 1)
	// huge IDs are not expected to equal but should reuse the freed ID.
	if p2.Entry() != p1.Entry() {
		t.Errorf("expected huge ID reuse: p1.Entry=%d p2.Entry=%d", p1.Entry(), p2.Entry())
	}
}

func TestAllocator_WriteIntoClassBoundary(t *testing.T) {
	a := NewAllocator()
	// Exactly fills a 64-byte class.
	p := a.Alloc(64)
	payload := bytes.Repeat([]byte{0xAB}, 64)
	a.Write(p, payload)
	if got := a.Read(p); !bytes.Equal(got, payload) {
		t.Errorf("boundary round-trip failed")
	}
	if a.Capacity(p) != 64 {
		t.Errorf("Capacity = %d, want 64", a.Capacity(p))
	}
}

func TestAllocator_StatsTracking(t *testing.T) {
	a := NewAllocator()
	// Initial counters: the reserved slot-0 bypasses Alloc() so the
	// counter starts at zero from the caller's perspective.
	s := a.Stats()
	if s.TotalAllocs != 0 || s.TotalFrees != 0 {
		t.Errorf("initial stats: allocs=%d frees=%d, want 0/0", s.TotalAllocs, s.TotalFrees)
	}
	for i := 0; i < 5; i++ {
		p := a.Alloc(16)
		a.Free(p)
	}
	s = a.Stats()
	if s.TotalAllocs != 5 || s.TotalFrees != 5 {
		t.Errorf("after 5 round-trips: allocs=%d frees=%d, want 5/5",
			s.TotalAllocs, s.TotalFrees)
	}
}

func TestAllocator_MixedSizes(t *testing.T) {
	a := NewAllocator()
	sizes := []uint32{1, 63, 64, 65, 127, 128, 255, 1024, 4096, 65536, 65537, 200_000}
	ps := make([]SlabPointer, len(sizes))
	payloads := make([][]byte, len(sizes))
	for i, sz := range sizes {
		ps[i] = a.Alloc(sz)
		payloads[i] = bytes.Repeat([]byte{byte(i + 1)}, int(sz))
		a.Write(ps[i], payloads[i])
	}
	for i, p := range ps {
		got := a.Read(p)
		if !bytes.Equal(got, payloads[i]) {
			t.Errorf("size %d: round-trip mismatch", sizes[i])
		}
	}
}

func TestAllocator_WriteThenShrinkGrow(t *testing.T) {
	a := NewAllocator()
	// Write big, then smaller, then bigger again — within the same class
	// slot. valueLen should update; capacity stays constant.
	p := a.Alloc(200)
	a.Write(p, bytes.Repeat([]byte{1}, 200))
	if len(a.Read(p)) != 200 {
		t.Fatal("initial write length wrong")
	}
	a.Write(p, []byte("x"))
	if got := a.Read(p); len(got) != 1 || got[0] != 'x' {
		t.Fatal("shrink write failed")
	}
	a.Write(p, bytes.Repeat([]byte{2}, 150))
	if len(a.Read(p)) != 150 {
		t.Fatal("grow write failed")
	}
}
