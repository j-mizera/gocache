package packed_test

import (
	"reflect"
	"testing"

	"gocache/pkg/cache"
	"gocache/pkg/cache/packed"
)

func TestZSetNewIsEmpty(t *testing.T) {
	buf := packed.ZSetNew()
	if n, _ := packed.ZSetLen(buf); n != 0 {
		t.Fatalf("ZSetLen = %d; want 0", n)
	}
}

func TestZSetAddMaintainsSortedOrder(t *testing.T) {
	buf := packed.ZSetNew()
	// Insert out of order; ZSet should store by (score asc, member asc).
	pairs := []struct {
		score  float64
		member string
	}{
		{3.0, "c"},
		{1.0, "a"},
		{2.0, "b"},
		{2.0, "aa"}, // tie on score — lex order breaks it
		{5.0, "z"},
	}
	for _, p := range pairs {
		var err error
		buf, _, _, _, err = packed.ZSetAdd(buf, p.member, p.score, 100, 100)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := packed.ZSetRangeByIndex(buf, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	want := []cache.ScoredMember{
		{Score: 1.0, Member: "a"},
		{Score: 2.0, Member: "aa"},
		{Score: 2.0, Member: "b"},
		{Score: 3.0, Member: "c"},
		{Score: 5.0, Member: "z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("range = %v; want %v", got, want)
	}
}

func TestZSetAddDuplicateUpdatesScore(t *testing.T) {
	buf := packed.ZSetNew()
	buf, added, _, _, _ := packed.ZSetAdd(buf, "a", 1.0, 100, 100)
	if !added {
		t.Fatal("first add should add")
	}
	buf, added, changed, _, _ := packed.ZSetAdd(buf, "a", 5.0, 100, 100)
	if added || !changed {
		t.Fatalf("update = added=%v changed=%v; want false,true", added, changed)
	}
	score, found, _ := packed.ZSetScoreOf(buf, "a")
	if !found || score != 5.0 {
		t.Errorf("score = %v found=%v; want 5.0, true", score, found)
	}
	if n, _ := packed.ZSetLen(buf); n != 1 {
		t.Errorf("len = %d; want 1", n)
	}
}

func TestZSetAddDuplicateSameScoreIsNoop(t *testing.T) {
	buf := packed.ZSetNew()
	buf, _, _, _, _ = packed.ZSetAdd(buf, "a", 1.0, 100, 100)
	_, added, changed, _, _ := packed.ZSetAdd(buf, "a", 1.0, 100, 100)
	if added || changed {
		t.Errorf("same score: added=%v changed=%v; want false,false", added, changed)
	}
}

func TestZSetRemove(t *testing.T) {
	buf := packed.ZSetNew()
	for _, p := range []struct {
		s float64
		m string
	}{{1, "a"}, {2, "b"}, {3, "c"}} {
		buf, _, _, _, _ = packed.ZSetAdd(buf, p.m, p.s, 100, 100)
	}
	buf, removed, err := packed.ZSetRemove(buf, "b")
	if err != nil || !removed {
		t.Fatalf("remove b = %v, %v", removed, err)
	}
	if n, _ := packed.ZSetLen(buf); n != 2 {
		t.Errorf("len after remove = %d; want 2", n)
	}
	_, found, _ := packed.ZSetScoreOf(buf, "b")
	if found {
		t.Errorf("b still present after remove")
	}
}

func TestZSetRank(t *testing.T) {
	buf := packed.ZSetNew()
	for _, p := range []struct {
		s float64
		m string
	}{{1, "a"}, {2, "b"}, {3, "c"}, {4, "d"}} {
		buf, _, _, _, _ = packed.ZSetAdd(buf, p.m, p.s, 100, 100)
	}
	cases := []struct {
		member    string
		wantRank  int
		wantScore float64
		wantFound bool
	}{
		{"a", 0, 1.0, true},
		{"b", 1, 2.0, true},
		{"d", 3, 4.0, true},
		{"nope", 0, 0, false},
	}
	for _, c := range cases {
		rank, score, found, err := packed.ZSetRank(buf, c.member)
		if err != nil {
			t.Fatal(err)
		}
		if found != c.wantFound || (found && (rank != c.wantRank || score != c.wantScore)) {
			t.Errorf("ZSetRank(%q) = %d, %v, %v; want %d, %v, %v",
				c.member, rank, score, found, c.wantRank, c.wantScore, c.wantFound)
		}
	}
}

func TestZSetRangeByScore(t *testing.T) {
	buf := packed.ZSetNew()
	for _, p := range []struct {
		s float64
		m string
	}{{1, "a"}, {2, "b"}, {3, "c"}, {4, "d"}, {5, "e"}} {
		buf, _, _, _, _ = packed.ZSetAdd(buf, p.m, p.s, 100, 100)
	}
	got, err := packed.ZSetRangeByScore(buf, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []cache.ScoredMember{
		{Score: 2, Member: "b"},
		{Score: 3, Member: "c"},
		{Score: 4, Member: "d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("range = %v; want %v", got, want)
	}
}

func TestZSetCountByScore(t *testing.T) {
	buf := packed.ZSetNew()
	for i := 0; i < 10; i++ {
		buf, _, _, _, _ = packed.ZSetAdd(buf, string(rune('a'+i)), float64(i), 100, 100)
	}
	n, err := packed.ZSetCountByScore(buf, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("count = %d; want 5", n)
	}
}

func TestZSetPromotionOnCount(t *testing.T) {
	buf := packed.ZSetNew()
	for i := 0; i < 3; i++ {
		var prom bool
		buf, _, _, prom, _ = packed.ZSetAdd(buf, string(rune('a'+i)), float64(i), 3, 100)
		if prom {
			t.Fatalf("early promotion at i=%d", i)
		}
	}
	_, _, _, prom, err := packed.ZSetAdd(buf, "d", 4, 3, 100)
	if err != nil || !prom {
		t.Fatalf("expected promotion at 4th entry: prom=%v err=%v", prom, err)
	}
}

func TestZSetPromotionOnMemberLength(t *testing.T) {
	_, _, _, prom, err := packed.ZSetAdd(packed.ZSetNew(), "12345", 1, 100, 5)
	if err != nil || prom {
		t.Fatalf("early promotion at len=5: prom=%v err=%v", prom, err)
	}
	_, _, _, prom, err = packed.ZSetAdd(packed.ZSetNew(), "123456", 1, 100, 5)
	if err != nil || !prom {
		t.Fatalf("expected promotion at len>5: prom=%v err=%v", prom, err)
	}
}

func TestZSetNaNRejected(t *testing.T) {
	_, _, _, _, err := packed.ZSetAdd(packed.ZSetNew(), "x", nanValue(), 100, 100)
	if err == nil {
		t.Errorf("expected NaN rejection; got nil")
	}
}

func nanValue() float64 {
	var zero float64
	return zero / zero
}

func TestZSetToNativeRoundTrip(t *testing.T) {
	buf := packed.ZSetNew()
	for _, p := range []struct {
		s float64
		m string
	}{{3, "c"}, {1, "a"}, {2, "b"}} {
		buf, _, _, _, _ = packed.ZSetAdd(buf, p.m, p.s, 100, 100)
	}
	z, err := packed.ZSetToNative(buf)
	if err != nil {
		t.Fatal(err)
	}
	pairs := z.GetSortedMembers()
	want := []cache.ScoredMember{
		{Score: 1, Member: "a"},
		{Score: 2, Member: "b"},
		{Score: 3, Member: "c"},
	}
	if !reflect.DeepEqual(pairs, want) {
		t.Errorf("round-trip = %v; want %v", pairs, want)
	}
}
