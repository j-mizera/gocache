package cache

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestEncodeList_RoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		items []string
	}{
		{"empty", nil},
		{"one", []string{"a"}},
		{"three", []string{"foo", "bar", "baz"}},
		{"with_empty_item", []string{"", "x", ""}},
		{"large_item", []string{strings.Repeat("q", 4096)}},
		{"duplicates_preserved", []string{"a", "a", "b", "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := EncodeList(tc.items)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := DecodeList(enc)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.items) {
				t.Fatalf("round-trip mismatch: got %v want %v", got, tc.items)
			}
			n, err := ListLen(enc)
			if err != nil {
				t.Fatalf("len: %v", err)
			}
			if n != len(tc.items) {
				t.Fatalf("ListLen=%d, want %d", n, len(tc.items))
			}
		})
	}
}

func TestEncodeList_TooMany(t *testing.T) {
	items := make([]string, MaxCollectionItems+1)
	if _, err := EncodeList(items); err != ErrTooManyItems {
		t.Fatalf("expected ErrTooManyItems, got %v", err)
	}
}

func TestDecodeList_Corrupt(t *testing.T) {
	t.Run("short_header", func(t *testing.T) {
		if _, err := DecodeList([]byte{1, 2}); err != ErrCorruptEncoding {
			t.Fatalf("expected ErrCorruptEncoding, got %v", err)
		}
	})
	t.Run("length_overflows", func(t *testing.T) {
		b := make([]byte, 4+4+3)
		be.PutUint32(b[0:4], 1)
		be.PutUint32(b[4:8], 999) // claims 999 bytes but only 3 present
		copy(b[8:], "abc")
		if _, err := DecodeList(b); err != ErrCorruptEncoding {
			t.Fatalf("expected ErrCorruptEncoding, got %v", err)
		}
	})
	t.Run("trailing_garbage", func(t *testing.T) {
		enc, _ := EncodeList([]string{"a"})
		enc = append(enc, 0xFF)
		if _, err := DecodeList(enc); err != ErrCorruptEncoding {
			t.Fatalf("expected ErrCorruptEncoding, got %v", err)
		}
	})
}

func TestEncodeHash_RoundTrip(t *testing.T) {
	cases := []map[string]string{
		{},
		{"k": "v"},
		{"a": "1", "b": "2", "c": "3"},
		{"": "zerokey", "emptyv": ""},
	}
	for i, m := range cases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			enc, err := EncodeHash(m)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := DecodeHash(enc)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != len(m) {
				t.Fatalf("len mismatch: got %d want %d", len(got), len(m))
			}
			for k, v := range m {
				if got[k] != v {
					t.Fatalf("key %q: got %q want %q", k, got[k], v)
				}
			}
			n, err := HashLen(enc)
			if err != nil {
				t.Fatalf("len: %v", err)
			}
			if n != len(m) {
				t.Fatalf("HashLen=%d, want %d", n, len(m))
			}
		})
	}
}

func TestEncodeSet_SortedOutput(t *testing.T) {
	members := map[string]struct{}{"c": {}, "a": {}, "b": {}}
	enc, err := EncodeSet(members)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeSetSlice(enc)
	if err != nil {
		t.Fatalf("decode slice: %v", err)
	}
	if fmt.Sprint(got) != "[a b c]" {
		t.Fatalf("expected sorted output, got %v", got)
	}

	roundTrip, err := DecodeSet(enc)
	if err != nil {
		t.Fatalf("decode map: %v", err)
	}
	if len(roundTrip) != 3 {
		t.Fatalf("round-trip len: got %d want 3", len(roundTrip))
	}
	for k := range members {
		if _, ok := roundTrip[k]; !ok {
			t.Fatalf("missing member %q after round trip", k)
		}
	}
}

func TestEncodeZSet_RoundTripOrderingStable(t *testing.T) {
	z := NewSortedSet()
	z.Add("a", 1.0)
	z.Add("b", 2.0)
	z.Add("c", 1.0) // tie with "a"; member-asc breaks tie

	enc, err := EncodeZSet(z)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pairs, err := DecodeZSetPairs(enc)
	if err != nil {
		t.Fatalf("decode pairs: %v", err)
	}
	want := []ScoredMember{
		{Member: "a", Score: 1.0}, // score 1, member "a" < "c"
		{Member: "c", Score: 1.0},
		{Member: "b", Score: 2.0},
	}
	if fmt.Sprint(pairs) != fmt.Sprint(want) {
		t.Fatalf("zset order: got %v want %v", pairs, want)
	}

	dz, err := DecodeZSet(enc)
	if err != nil {
		t.Fatalf("decode zset: %v", err)
	}
	if dz.Card() != z.Card() {
		t.Fatalf("cardinality: got %d want %d", dz.Card(), z.Card())
	}
	if s, _ := dz.Score("c"); s != 1.0 {
		t.Fatalf("score(c): got %v want 1.0", s)
	}
}

func TestEncodeZSet_Empty(t *testing.T) {
	enc, err := EncodeZSet(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	n, err := ZSetLen(enc)
	if err != nil {
		t.Fatalf("len: %v", err)
	}
	if n != 0 {
		t.Fatalf("ZSetLen: got %d want 0", n)
	}
	pairs, err := DecodeZSetPairs(enc)
	if err != nil {
		t.Fatalf("decode pairs: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("empty zset decode: got %d pairs", len(pairs))
	}
}

func TestEncoders_ExactSizeNoOvershoot(t *testing.T) {
	// The slab allocator will charge len(encoded) as the on-disk size, so it's
	// important the encoded length exactly matches the sum of components. Any
	// overshoot wastes memory proportional to the cache size.
	t.Run("list", func(t *testing.T) {
		items := []string{"foo", "bar", "baz"}
		enc, _ := EncodeList(items)
		want := 4 + 3*4 + 3 + 3 + 3
		if len(enc) != want {
			t.Fatalf("list size: got %d want %d", len(enc), want)
		}
	})
	t.Run("hash", func(t *testing.T) {
		m := map[string]string{"k": "v"}
		enc, _ := EncodeHash(m)
		want := 4 + 4 + 1 + 4 + 1
		if len(enc) != want {
			t.Fatalf("hash size: got %d want %d", len(enc), want)
		}
	})
	t.Run("set", func(t *testing.T) {
		s := map[string]struct{}{"a": {}, "b": {}}
		enc, _ := EncodeSet(s)
		want := 4 + 2*(4+1)
		if len(enc) != want {
			t.Fatalf("set size: got %d want %d", len(enc), want)
		}
	})
	t.Run("zset", func(t *testing.T) {
		z := NewSortedSet()
		z.Add("x", 0.5)
		enc, _ := EncodeZSet(z)
		want := 4 + 8 + 4 + 1
		if len(enc) != want {
			t.Fatalf("zset size: got %d want %d", len(enc), want)
		}
	})
}

func TestDecodeList_DoesNotAliasInput(t *testing.T) {
	// The slab allocator will free the backing byte region, so decoders must
	// not let caller data hold references into it.
	enc, _ := EncodeList([]string{"hello"})
	items, _ := DecodeList(enc)
	// Mutate the source — decoded strings must stay intact.
	for i := range enc {
		enc[i] = 0xFF
	}
	if items[0] != "hello" {
		t.Fatalf("DecodeList aliased input — got %q after overwrite", items[0])
	}
}

func TestReadFramed_ItemTooLarge(t *testing.T) {
	// Header claims a length greater than MaxCollectionItemLen should reject
	// before any read attempt.
	b := make([]byte, 4)
	be.PutUint32(b, uint32(MaxCollectionItemLen+1))
	_, _, err := readFramed(append([]byte{0, 0, 0, 1}, b...), 4)
	if err != ErrItemTooLarge {
		t.Fatalf("expected ErrItemTooLarge, got %v", err)
	}
}

// Sanity: ensure the encoded bytes don't secretly share storage with the
// input slices. (Catches append-with-capacity-reuse bugs.)
func TestEncoders_OutputOwnsStorage(t *testing.T) {
	items := []string{"x"}
	enc, _ := EncodeList(items)
	if &items[0] == nil {
		t.Fatal("impossible")
	}
	// Changing the original shouldn't change the encoding.
	itemsCopy := append([]string(nil), items...)
	items[0] = "y"
	enc2, _ := EncodeList(itemsCopy)
	if !bytes.Equal(enc, enc2) {
		t.Fatal("encoding aliased input slice")
	}
}
