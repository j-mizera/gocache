package packed_test

import (
	"reflect"
	"testing"

	"gocache/pkg/cache/packed"
)

func TestSetNewIsEmpty(t *testing.T) {
	buf := packed.SetNew()
	if n, _ := packed.SetLen(buf); n != 0 {
		t.Fatalf("SetLen = %d; want 0", n)
	}
}

func TestSetAddMaintainsSortedOrder(t *testing.T) {
	buf := packed.SetNew()
	for _, m := range []string{"c", "a", "b", "e", "d"} {
		var err error
		buf, _, _, err = packed.SetAdd(buf, m, 100, 100)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := packed.SetMembers(buf)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c", "d", "e"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("members = %v; want %v", got, want)
	}
}

func TestSetAddDuplicateIsNoop(t *testing.T) {
	buf, added, _, err := packed.SetAdd(packed.SetNew(), "x", 100, 100)
	if err != nil || !added {
		t.Fatalf("first add: %v, %v", added, err)
	}
	buf2, added, _, err := packed.SetAdd(buf, "x", 100, 100)
	if err != nil || added {
		t.Fatalf("duplicate add: added=%v err=%v; want false, nil", added, err)
	}
	if len(buf2) != len(buf) {
		t.Errorf("duplicate add grew the buffer")
	}
}

func TestSetContains(t *testing.T) {
	buf := packed.SetNew()
	for _, m := range []string{"alice", "bob", "carol"} {
		buf, _, _, _ = packed.SetAdd(buf, m, 100, 100)
	}
	for _, m := range []string{"alice", "bob", "carol"} {
		ok, err := packed.SetContains(buf, m)
		if err != nil || !ok {
			t.Errorf("SetContains(%q) = %v, %v; want true, nil", m, ok, err)
		}
	}
	ok, err := packed.SetContains(buf, "missing")
	if err != nil || ok {
		t.Errorf("SetContains(missing) = %v, %v; want false, nil", ok, err)
	}
}

func TestSetRemove(t *testing.T) {
	buf := packed.SetNew()
	for _, m := range []string{"a", "b", "c"} {
		buf, _, _, _ = packed.SetAdd(buf, m, 100, 100)
	}
	buf, removed, err := packed.SetRemove(buf, "b")
	if err != nil || !removed {
		t.Fatalf("SetRemove(b) = %v, %v; want true, nil", removed, err)
	}
	got, _ := packed.SetMembers(buf)
	if !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("after remove = %v; want [a c]", got)
	}

	buf2, removed, err := packed.SetRemove(buf, "missing")
	if err != nil || removed {
		t.Errorf("remove missing: %v, %v; want false, nil", removed, err)
	}
	if len(buf2) != len(buf) {
		t.Errorf("remove missing grew buffer")
	}
}

func TestSetPromotionOnCount(t *testing.T) {
	buf := packed.SetNew()
	for _, m := range []string{"a", "b", "c"} {
		var prom bool
		buf, _, prom, _ = packed.SetAdd(buf, m, 3, 100)
		if prom {
			t.Fatalf("early promotion at %q", m)
		}
	}
	_, _, prom, err := packed.SetAdd(buf, "d", 3, 100)
	if err != nil || !prom {
		t.Fatalf("expected promotion at 4th entry: prom=%v err=%v", prom, err)
	}
}

func TestSetPromotionOnMemberLength(t *testing.T) {
	_, _, prom, err := packed.SetAdd(packed.SetNew(), "12345", 100, 5)
	if err != nil || prom {
		t.Fatalf("early promotion at len=5 maxLen=5: prom=%v err=%v", prom, err)
	}
	_, _, prom, err = packed.SetAdd(packed.SetNew(), "123456", 100, 5)
	if err != nil || !prom {
		t.Fatalf("expected promotion for len=6 > 5: prom=%v err=%v", prom, err)
	}
}

func TestSetFromMapRoundTrip(t *testing.T) {
	src := map[string]struct{}{"beta": {}, "alpha": {}, "gamma": {}}
	buf, err := packed.SetFromMap(src)
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip should preserve the set.
	got, err := packed.SetToMap(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(src) {
		t.Fatalf("round-trip len = %d; want %d", len(got), len(src))
	}
	for k := range src {
		if _, ok := got[k]; !ok {
			t.Errorf("missing member after round-trip: %q", k)
		}
	}
	// Members should come back sorted via SetMembers.
	members, _ := packed.SetMembers(buf)
	if !reflect.DeepEqual(members, []string{"alpha", "beta", "gamma"}) {
		t.Errorf("members = %v; want [alpha beta gamma]", members)
	}
}
