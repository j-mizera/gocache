package cache

import (
	"encoding/binary"
	"errors"
	"math"
	"sort"
)

// Flat byte layouts for each non-scalar value type. All integers are big-endian
// so snapshots are portable. Layouts intentionally keep a uniform shape —
// [count u32][len u32, bytes]... — so the decoder can walk forward without
// knowing the concrete type beyond ValueType.
//
// Size of any encoded value is exactly len(encoded), so estimateSize is no
// longer needed; the slab allocator in later phases charges the encoded
// size byte-for-byte.
//
//   string  : raw bytes, no framing
//   list    : [count u32] [len u32] [item] ... [len u32] [item]
//             order is left-to-right (index 0 = leftmost)
//   hash    : [count u32] ([fieldLen u32] [field] [valueLen u32] [value]) *
//             iteration order is whatever encode saw — not stable across
//             mutations, matching today's map-backed semantics
//   set     : [count u32] [memLen u32] [member] ... [memLen u32] [member]
//             members are stored in lexicographic ascending order so
//             SINTER/SUNION/SDIFF can do O(n+m) merges instead of n^2 scans
//   zset    : [count u32] ([score f64] [memLen u32] [member]) *
//             sorted by (score ascending, member ascending) — matches
//             SortedSet.GetSortedMembers so ZRANGE walks linearly
//
// Bounds:
//   MaxCollectionItems caps the element count of list/hash/set/zset.
//   MaxCollectionItemLen caps the length of any single member or value.
// These match resp's maxArrayElements / maxBulkStringBytes.

const (
	MaxCollectionItems   = 1 << 20       // 1M items per collection
	MaxCollectionItemLen = 512 << 20     // 512 MiB per item
	lenPrefixBytes       = 4             // every length prefix is u32
	scoreBytes           = 8             // sorted-set score is a float64
)

var (
	ErrCorruptEncoding = errors.New("cache/encoding: corrupt or truncated payload")
	ErrTooManyItems    = errors.New("cache/encoding: collection has too many items")
	ErrItemTooLarge    = errors.New("cache/encoding: collection item exceeds maximum length")
	ErrOddHashFields   = errors.New("cache/encoding: hash payload has odd number of field/value halves")
)

// be is the canonical encoder — aliased for readability.
var be = binary.BigEndian

// ---------- Lists ----------

// EncodeList returns the flat encoding of items. Items is stored in order;
// callers supply LPUSH-style prepends by prepending before calling this.
func EncodeList(items []string) ([]byte, error) {
	if len(items) > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	size := lenPrefixBytes
	for _, it := range items {
		if len(it) > MaxCollectionItemLen {
			return nil, ErrItemTooLarge
		}
		size += lenPrefixBytes + len(it)
	}
	out := make([]byte, size)
	be.PutUint32(out[0:4], uint32(len(items)))
	pos := lenPrefixBytes
	for _, it := range items {
		be.PutUint32(out[pos:pos+4], uint32(len(it)))
		pos += lenPrefixBytes
		pos += copy(out[pos:], it)
	}
	return out, nil
}

// DecodeList reverses EncodeList. The returned slice shares no memory with b.
func DecodeList(b []byte) ([]string, error) {
	if len(b) < lenPrefixBytes {
		return nil, ErrCorruptEncoding
	}
	count := be.Uint32(b[0:4])
	if count > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	out := make([]string, count)
	pos := lenPrefixBytes
	for i := uint32(0); i < count; i++ {
		if pos+lenPrefixBytes > len(b) {
			return nil, ErrCorruptEncoding
		}
		n := be.Uint32(b[pos : pos+4])
		pos += lenPrefixBytes
		if n > MaxCollectionItemLen || pos+int(n) > len(b) {
			return nil, ErrCorruptEncoding
		}
		out[i] = string(b[pos : pos+int(n)])
		pos += int(n)
	}
	if pos != len(b) {
		return nil, ErrCorruptEncoding
	}
	return out, nil
}

// ListLen reads only the count prefix — O(1), no allocations.
func ListLen(b []byte) (int, error) {
	if len(b) < lenPrefixBytes {
		return 0, ErrCorruptEncoding
	}
	return int(be.Uint32(b[0:4])), nil
}

// ---------- Hashes ----------

// EncodeHash serialises a field→value map. Iteration order follows Go map
// iteration order and is NOT stable — matching today's hash semantics.
func EncodeHash(m map[string]string) ([]byte, error) {
	if len(m) > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	size := lenPrefixBytes
	for k, v := range m {
		if len(k) > MaxCollectionItemLen || len(v) > MaxCollectionItemLen {
			return nil, ErrItemTooLarge
		}
		size += 2*lenPrefixBytes + len(k) + len(v)
	}
	out := make([]byte, size)
	be.PutUint32(out[0:4], uint32(len(m)))
	pos := lenPrefixBytes
	for k, v := range m {
		be.PutUint32(out[pos:pos+4], uint32(len(k)))
		pos += lenPrefixBytes
		pos += copy(out[pos:], k)
		be.PutUint32(out[pos:pos+4], uint32(len(v)))
		pos += lenPrefixBytes
		pos += copy(out[pos:], v)
	}
	return out, nil
}

// DecodeHash reverses EncodeHash.
func DecodeHash(b []byte) (map[string]string, error) {
	if len(b) < lenPrefixBytes {
		return nil, ErrCorruptEncoding
	}
	count := be.Uint32(b[0:4])
	if count > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	out := make(map[string]string, count)
	pos := lenPrefixBytes
	for i := uint32(0); i < count; i++ {
		k, nk, err := readFramed(b, pos)
		if err != nil {
			return nil, err
		}
		pos = nk
		v, nv, err := readFramed(b, pos)
		if err != nil {
			return nil, err
		}
		pos = nv
		out[k] = v
	}
	if pos != len(b) {
		return nil, ErrCorruptEncoding
	}
	return out, nil
}

// HashLen reads only the count prefix.
func HashLen(b []byte) (int, error) {
	if len(b) < lenPrefixBytes {
		return 0, ErrCorruptEncoding
	}
	return int(be.Uint32(b[0:4])), nil
}

// ---------- Sets ----------

// EncodeSet stores the members in lexicographic order so set operations can
// do merge-style walks in O(n+m). Duplicate members are dropped.
func EncodeSet(members map[string]struct{}) ([]byte, error) {
	if len(members) > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	sorted := make([]string, 0, len(members))
	for m := range members {
		if len(m) > MaxCollectionItemLen {
			return nil, ErrItemTooLarge
		}
		sorted = append(sorted, m)
	}
	sort.Strings(sorted)

	size := lenPrefixBytes
	for _, m := range sorted {
		size += lenPrefixBytes + len(m)
	}
	out := make([]byte, size)
	be.PutUint32(out[0:4], uint32(len(sorted)))
	pos := lenPrefixBytes
	for _, m := range sorted {
		be.PutUint32(out[pos:pos+4], uint32(len(m)))
		pos += lenPrefixBytes
		pos += copy(out[pos:], m)
	}
	return out, nil
}

// DecodeSet reverses EncodeSet. The returned map preserves only membership;
// ordering information is lost (a map was asked for, a map is returned).
func DecodeSet(b []byte) (map[string]struct{}, error) {
	if len(b) < lenPrefixBytes {
		return nil, ErrCorruptEncoding
	}
	count := be.Uint32(b[0:4])
	if count > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	out := make(map[string]struct{}, count)
	pos := lenPrefixBytes
	for i := uint32(0); i < count; i++ {
		m, n, err := readFramed(b, pos)
		if err != nil {
			return nil, err
		}
		pos = n
		out[m] = struct{}{}
	}
	if pos != len(b) {
		return nil, ErrCorruptEncoding
	}
	return out, nil
}

// DecodeSetSlice decodes directly to the sorted []string that EncodeSet stored.
// Cheaper than DecodeSet→map→sort when the caller just needs iteration.
func DecodeSetSlice(b []byte) ([]string, error) {
	if len(b) < lenPrefixBytes {
		return nil, ErrCorruptEncoding
	}
	count := be.Uint32(b[0:4])
	if count > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	out := make([]string, count)
	pos := lenPrefixBytes
	for i := uint32(0); i < count; i++ {
		m, n, err := readFramed(b, pos)
		if err != nil {
			return nil, err
		}
		pos = n
		out[i] = m
	}
	if pos != len(b) {
		return nil, ErrCorruptEncoding
	}
	return out, nil
}

// SetLen reads only the count prefix.
func SetLen(b []byte) (int, error) {
	if len(b) < lenPrefixBytes {
		return 0, ErrCorruptEncoding
	}
	return int(be.Uint32(b[0:4])), nil
}

// ---------- Sorted Sets ----------

// EncodeZSet encodes a SortedSet. Entries are written in (score asc, member
// asc) order so ZRANGE walks linearly and ZRANK can binary-search later.
func EncodeZSet(z *SortedSet) ([]byte, error) {
	if z == nil {
		// Empty encoding: count = 0.
		return EncodeZSetPairs(nil)
	}
	return EncodeZSetPairs(z.GetSortedMembers())
}

// EncodeZSetPairs accepts pre-sorted ScoredMembers. The caller is responsible
// for ensuring the order matches the contract above.
func EncodeZSetPairs(pairs []ScoredMember) ([]byte, error) {
	if len(pairs) > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	size := lenPrefixBytes
	for _, p := range pairs {
		if len(p.Member) > MaxCollectionItemLen {
			return nil, ErrItemTooLarge
		}
		size += scoreBytes + lenPrefixBytes + len(p.Member)
	}
	out := make([]byte, size)
	be.PutUint32(out[0:4], uint32(len(pairs)))
	pos := lenPrefixBytes
	for _, p := range pairs {
		be.PutUint64(out[pos:pos+scoreBytes], math.Float64bits(p.Score))
		pos += scoreBytes
		be.PutUint32(out[pos:pos+4], uint32(len(p.Member)))
		pos += lenPrefixBytes
		pos += copy(out[pos:], p.Member)
	}
	return out, nil
}

// DecodeZSet reverses EncodeZSet into the in-memory SortedSet shape.
func DecodeZSet(b []byte) (*SortedSet, error) {
	pairs, err := DecodeZSetPairs(b)
	if err != nil {
		return nil, err
	}
	z := NewSortedSet()
	for _, p := range pairs {
		z.Add(p.Member, p.Score)
	}
	return z, nil
}

// DecodeZSetPairs returns the pairs in encoded (sorted) order — cheaper than
// materialising a SortedSet when the caller just needs a linear walk.
func DecodeZSetPairs(b []byte) ([]ScoredMember, error) {
	if len(b) < lenPrefixBytes {
		return nil, ErrCorruptEncoding
	}
	count := be.Uint32(b[0:4])
	if count > MaxCollectionItems {
		return nil, ErrTooManyItems
	}
	out := make([]ScoredMember, count)
	pos := lenPrefixBytes
	for i := uint32(0); i < count; i++ {
		if pos+scoreBytes > len(b) {
			return nil, ErrCorruptEncoding
		}
		score := math.Float64frombits(be.Uint64(b[pos : pos+scoreBytes]))
		pos += scoreBytes
		m, n, err := readFramed(b, pos)
		if err != nil {
			return nil, err
		}
		pos = n
		out[i] = ScoredMember{Member: m, Score: score}
	}
	if pos != len(b) {
		return nil, ErrCorruptEncoding
	}
	return out, nil
}

// ZSetLen reads only the count prefix.
func ZSetLen(b []byte) (int, error) {
	if len(b) < lenPrefixBytes {
		return 0, ErrCorruptEncoding
	}
	return int(be.Uint32(b[0:4])), nil
}

// ---------- Internal helpers ----------

// readFramed consumes one [u32 length][bytes] frame starting at pos and
// returns the string plus the position after the frame.
func readFramed(b []byte, pos int) (string, int, error) {
	if pos+lenPrefixBytes > len(b) {
		return "", 0, ErrCorruptEncoding
	}
	n := be.Uint32(b[pos : pos+lenPrefixBytes])
	pos += lenPrefixBytes
	if n > MaxCollectionItemLen {
		return "", 0, ErrItemTooLarge
	}
	if pos+int(n) > len(b) {
		return "", 0, ErrCorruptEncoding
	}
	s := string(b[pos : pos+int(n)])
	return s, pos + int(n), nil
}

