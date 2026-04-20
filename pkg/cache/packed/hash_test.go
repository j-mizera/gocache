package packed_test

import (
	"errors"
	"testing"

	"gocache/pkg/cache"
	"gocache/pkg/cache/packed"
)

func TestHashNewIsEmpty(t *testing.T) {
	buf := packed.HashNew()
	if got, err := packed.HashLen(buf); err != nil || got != 0 {
		t.Fatalf("HashLen(HashNew) = %d, %v; want 0, nil", got, err)
	}
}

func TestHashSetAppendThenGet(t *testing.T) {
	buf := packed.HashNew()
	buf, added, prom, err := packed.HashSet(buf, "field1", "hello", 100, 100)
	if err != nil || !added || prom {
		t.Fatalf("HashSet(new) = added=%v prom=%v err=%v; want added=true prom=false", added, prom, err)
	}

	v, found, err := packed.HashGet(buf, "field1")
	if err != nil || !found || string(v) != "hello" {
		t.Fatalf("HashGet(field1) = %q, %v, %v; want \"hello\", true, nil", v, found, err)
	}

	if got, _ := packed.HashLen(buf); got != 1 {
		t.Fatalf("HashLen = %d; want 1", got)
	}
}

func TestHashSetSameSizeUpdateNoRealloc(t *testing.T) {
	buf := packed.HashNew()
	buf, _, _, _ = packed.HashSet(buf, "f", "xxxxx", 100, 100)
	origPtr := &buf[0]

	buf2, added, prom, err := packed.HashSet(buf, "f", "yyyyy", 100, 100)
	if err != nil || added || prom {
		t.Fatalf("HashSet(same-size) = added=%v prom=%v err=%v; want both false", added, prom, err)
	}
	if &buf2[0] != origPtr {
		t.Fatalf("same-size update should not reallocate buffer")
	}
	v, _, _ := packed.HashGet(buf2, "f")
	if string(v) != "yyyyy" {
		t.Fatalf("HashGet after same-size update = %q; want yyyyy", v)
	}
}

func TestHashSetSizeChangeReplacesValue(t *testing.T) {
	buf := packed.HashNew()
	buf, _, _, _ = packed.HashSet(buf, "a", "1", 100, 100)
	buf, _, _, _ = packed.HashSet(buf, "b", "2", 100, 100)
	buf, _, _, _ = packed.HashSet(buf, "c", "3", 100, 100)

	buf, added, _, err := packed.HashSet(buf, "b", "a much longer value", 100, 100)
	if err != nil || added {
		t.Fatalf("HashSet(size-change) = added=%v err=%v; want added=false", added, err)
	}

	if v, _, _ := packed.HashGet(buf, "a"); string(v) != "1" {
		t.Errorf("a = %q; want 1", v)
	}
	if v, _, _ := packed.HashGet(buf, "b"); string(v) != "a much longer value" {
		t.Errorf("b = %q; want \"a much longer value\"", v)
	}
	if v, _, _ := packed.HashGet(buf, "c"); string(v) != "3" {
		t.Errorf("c = %q; want 3", v)
	}

	if n, _ := packed.HashLen(buf); n != 3 {
		t.Errorf("HashLen = %d; want 3", n)
	}
}

func TestHashDelete(t *testing.T) {
	buf := packed.HashNew()
	fields := []struct{ k, v string }{{"a", "1"}, {"b", "2"}, {"c", "3"}}
	for _, p := range fields {
		buf, _, _, _ = packed.HashSet(buf, p.k, p.v, 100, 100)
	}

	buf, removed, err := packed.HashDelete(buf, "b")
	if err != nil || !removed {
		t.Fatalf("HashDelete(b) = %v, %v; want true, nil", removed, err)
	}
	if n, _ := packed.HashLen(buf); n != 2 {
		t.Fatalf("HashLen after delete = %d; want 2", n)
	}

	if v, found, _ := packed.HashGet(buf, "b"); found {
		t.Errorf("HashGet(b) after delete = %q, found=true; want not found", v)
	}
	if v, _, _ := packed.HashGet(buf, "a"); string(v) != "1" {
		t.Errorf("a = %q; want 1", v)
	}
	if v, _, _ := packed.HashGet(buf, "c"); string(v) != "3" {
		t.Errorf("c = %q; want 3", v)
	}

	// Deleting missing field is a no-op.
	buf2, removed2, err := packed.HashDelete(buf, "nope")
	if err != nil || removed2 {
		t.Errorf("HashDelete(missing) = %v, %v; want false, nil", removed2, err)
	}
	if n, _ := packed.HashLen(buf2); n != 2 {
		t.Errorf("HashLen unchanged = %d; want 2", n)
	}
}

func TestHashPromotionOnEntryCount(t *testing.T) {
	buf := packed.HashNew()
	// maxEntries=3: fourth insertion should signal promotion.
	var prom bool
	for i, k := range []string{"a", "b", "c"} {
		buf, _, prom, _ = packed.HashSet(buf, k, "v", 3, 100)
		if prom {
			t.Fatalf("premature promotion at insert %d", i)
		}
	}
	buf, _, prom, _ = packed.HashSet(buf, "d", "v", 3, 100)
	if !prom {
		t.Fatalf("expected promotion on 4th entry with maxEntries=3")
	}
	_ = buf
}

func TestHashPromotionOnValueLength(t *testing.T) {
	buf := packed.HashNew()
	// maxValueLen=5: a 6-byte value triggers promotion.
	buf, _, prom, err := packed.HashSet(buf, "k", "12345", 100, 5)
	if err != nil || prom {
		t.Fatalf("premature promotion at value-len boundary: prom=%v err=%v", prom, err)
	}
	buf, _, prom, err = packed.HashSet(buf, "k", "123456", 100, 5)
	if err != nil || !prom {
		t.Fatalf("expected promotion for value > maxValueLen: prom=%v err=%v", prom, err)
	}
	_ = buf
}

func TestHashToMap(t *testing.T) {
	buf := packed.HashNew()
	buf, _, _, _ = packed.HashSet(buf, "a", "1", 100, 100)
	buf, _, _, _ = packed.HashSet(buf, "b", "2", 100, 100)
	buf, _, _, _ = packed.HashSet(buf, "c", "3", 100, 100)

	m, err := packed.HashToMap(buf)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "1", "b": "2", "c": "3"}
	if len(m) != len(want) {
		t.Fatalf("len = %d; want %d", len(m), len(want))
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("m[%q] = %q; want %q", k, m[k], v)
		}
	}
}

func TestHashFromMapRoundTrip(t *testing.T) {
	src := map[string]string{"a": "1", "bb": "22", "ccc": "333"}
	buf, err := packed.HashFromMap(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := packed.HashToMap(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(src) {
		t.Fatalf("round-trip len = %d; want %d", len(got), len(src))
	}
	for k, v := range src {
		if got[k] != v {
			t.Errorf("got[%q] = %q; want %q", k, got[k], v)
		}
	}
}

func TestHashRejectsOversizedFieldOrValue(t *testing.T) {
	big := make([]byte, cache.MaxCollectionItemLen+1)
	_, _, _, err := packed.HashSet(packed.HashNew(), string(big), "v", 100, 100)
	if !errors.Is(err, cache.ErrItemTooLarge) {
		t.Errorf("oversized field: err = %v; want ErrItemTooLarge", err)
	}
	_, _, _, err = packed.HashSet(packed.HashNew(), "k", string(big), 100, 100)
	if !errors.Is(err, cache.ErrItemTooLarge) {
		t.Errorf("oversized value: err = %v; want ErrItemTooLarge", err)
	}
}

func TestHashCorruptBuffer(t *testing.T) {
	_, err := packed.HashLen([]byte{0x00})
	if !errors.Is(err, cache.ErrCorruptEncoding) {
		t.Errorf("short header: err = %v; want ErrCorruptEncoding", err)
	}
}
