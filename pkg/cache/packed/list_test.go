package packed_test

import (
	"reflect"
	"testing"

	"gocache/pkg/cache/packed"
)

func mustListItems(t *testing.T, buf []byte) []string {
	t.Helper()
	got, err := packed.ListToSlice(buf)
	if err != nil {
		t.Fatalf("ListToSlice: %v", err)
	}
	return got
}

func TestListNewIsEmpty(t *testing.T) {
	buf := packed.ListNew()
	if got, _ := packed.ListLen(buf); got != 0 {
		t.Fatalf("ListLen = %d; want 0", got)
	}
}

func TestListAppendRightBasic(t *testing.T) {
	buf, _, err := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "c"}, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"a", "b", "c"}) {
		t.Fatalf("items = %v; want [a b c]", items)
	}
}

func TestListAppendLeftOrdering(t *testing.T) {
	// Redis LPUSH k a b c → list becomes [c, b, a].
	buf, _, err := packed.ListAppendLeft(packed.ListNew(), []string{"a", "b", "c"}, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"c", "b", "a"}) {
		t.Fatalf("LPUSH a b c = %v; want [c b a]", items)
	}
}

func TestListAppendLeftOntoExisting(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"x", "y"}, 8192)
	buf, _, _ = packed.ListAppendLeft(buf, []string{"a", "b"}, 8192)
	// Existing [x, y], then LPUSH a b → [b, a, x, y].
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"b", "a", "x", "y"}) {
		t.Fatalf("LPUSH a b onto [x y] = %v; want [b a x y]", items)
	}
}

func TestListPopLeftAndRight(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "c"}, 8192)
	buf, left, ok, err := packed.ListPopLeft(buf)
	if err != nil || !ok || string(left) != "a" {
		t.Fatalf("LPopLeft = %q ok=%v err=%v; want a true nil", left, ok, err)
	}
	buf, right, ok, err := packed.ListPopRight(buf)
	if err != nil || !ok || string(right) != "c" {
		t.Fatalf("ListPopRight = %q ok=%v err=%v; want c true nil", right, ok, err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"b"}) {
		t.Fatalf("after pops = %v; want [b]", items)
	}
}

func TestListPopFromEmptyIsNoop(t *testing.T) {
	buf, _, ok, err := packed.ListPopLeft(packed.ListNew())
	if err != nil || ok {
		t.Errorf("ListPopLeft(empty) ok=%v err=%v; want false nil", ok, err)
	}
	_, _, ok, err = packed.ListPopRight(buf)
	if err != nil || ok {
		t.Errorf("ListPopRight(empty) ok=%v err=%v; want false nil", ok, err)
	}
}

func TestListIndex(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "c", "d"}, 8192)
	cases := []struct {
		idx    int
		want   string
		found  bool
	}{
		{0, "a", true},
		{1, "b", true},
		{3, "d", true},
		{-1, "d", true},
		{-4, "a", true},
		{4, "", false},
		{-5, "", false},
	}
	for _, c := range cases {
		got, found, err := packed.ListIndex(buf, c.idx)
		if err != nil {
			t.Fatalf("idx=%d err=%v", c.idx, err)
		}
		if found != c.found || (found && string(got) != c.want) {
			t.Errorf("ListIndex(%d) = %q, %v; want %q, %v", c.idx, got, found, c.want, c.found)
		}
	}
}

func TestListRange(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "c", "d", "e"}, 8192)
	cases := []struct {
		start, stop int
		want        []string
	}{
		{0, 2, []string{"a", "b", "c"}},
		{0, -1, []string{"a", "b", "c", "d", "e"}},
		{-2, -1, []string{"d", "e"}},
		{3, 100, []string{"d", "e"}},
		{5, 10, nil},
		{-100, 1, []string{"a", "b"}},
	}
	for _, c := range cases {
		got, err := packed.ListRange(buf, c.start, c.stop)
		if err != nil {
			t.Fatalf("start=%d stop=%d err=%v", c.start, c.stop, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("LRANGE %d %d = %v; want %v", c.start, c.stop, got, c.want)
		}
	}
}

func TestListRemoveHead(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "a", "c", "a"}, 8192)
	buf, n, err := packed.ListRemove(buf, "a", 2)
	if err != nil || n != 2 {
		t.Fatalf("ListRemove(a,2) = %d, %v; want 2, nil", n, err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"b", "c", "a"}) {
		t.Errorf("items = %v; want [b c a]", items)
	}
}

func TestListRemoveTail(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "a", "c", "a"}, 8192)
	buf, n, err := packed.ListRemove(buf, "a", -2)
	if err != nil || n != 2 {
		t.Fatalf("ListRemove(a,-2) = %d, %v; want 2, nil", n, err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"a", "b", "c"}) {
		t.Errorf("items = %v; want [a b c]", items)
	}
}

func TestListRemoveAll(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "a", "c", "a"}, 8192)
	buf, n, err := packed.ListRemove(buf, "a", 0)
	if err != nil || n != 3 {
		t.Fatalf("ListRemove(a,0) = %d, %v; want 3, nil", n, err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"b", "c"}) {
		t.Errorf("items = %v; want [b c]", items)
	}
}

func TestListSetSameSize(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"aaa", "bbb", "ccc"}, 8192)
	buf, ok, err := packed.ListSet(buf, 1, "ZZZ")
	if err != nil || !ok {
		t.Fatalf("ListSet = %v, %v; want true, nil", ok, err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"aaa", "ZZZ", "ccc"}) {
		t.Errorf("items = %v; want [aaa ZZZ ccc]", items)
	}
}

func TestListSetSizeChange(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "c"}, 8192)
	buf, ok, err := packed.ListSet(buf, 1, "a much longer replacement")
	if err != nil || !ok {
		t.Fatalf("ListSet size-change = %v, %v; want true, nil", ok, err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"a", "a much longer replacement", "c"}) {
		t.Errorf("items = %v; want expected", items)
	}
}

func TestListSetOutOfRange(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a"}, 8192)
	_, ok, err := packed.ListSet(buf, 5, "x")
	if err != nil || ok {
		t.Errorf("out-of-range = %v, %v; want false, nil", ok, err)
	}
}

func TestListTrim(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "c", "d", "e"}, 8192)
	buf, err := packed.ListTrim(buf, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if items := mustListItems(t, buf); !reflect.DeepEqual(items, []string{"b", "c", "d"}) {
		t.Errorf("items = %v; want [b c d]", items)
	}
}

func TestListTrimEmptiesWhenStartGtStop(t *testing.T) {
	buf, _, _ := packed.ListAppendRight(packed.ListNew(), []string{"a", "b", "c"}, 8192)
	buf, err := packed.ListTrim(buf, 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := packed.ListLen(buf); n != 0 {
		t.Errorf("after trim out-of-range, len = %d; want 0", n)
	}
}

func TestListPromotionOnSize(t *testing.T) {
	buf := packed.ListNew()
	// Each of these entries uses 4-byte len prefix + 10-byte value = 14 bytes.
	// maxBytes = 50 → promotion triggers after a few appends.
	for i := 0; i < 3; i++ {
		var prom bool
		var err error
		buf, prom, err = packed.ListAppendRight(buf, []string{"abcdefghij"}, 50)
		if err != nil {
			t.Fatal(err)
		}
		_ = prom
	}
	if n, _ := packed.ListLen(buf); n != 3 {
		t.Fatalf("len = %d; want 3", n)
	}
	// Next append should report promotion.
	_, prom, err := packed.ListAppendRight(buf, []string{"abcdefghij"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !prom {
		t.Fatalf("expected promotion when size exceeds maxBytes")
	}
}

func TestListRoundTripFromSlice(t *testing.T) {
	src := []string{"x", "yy", "zzz", ""}
	buf, err := packed.ListFromSlice(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := packed.ListToSlice(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, src) {
		t.Errorf("round-trip = %v; want %v", got, src)
	}
}
