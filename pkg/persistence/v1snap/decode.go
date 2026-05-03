package v1snap

import (
	"encoding/binary"
	"fmt"
	"math"

	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
)

// decodeNativeValue is the inverse of encodeNativeValue. Returns the
// concrete Go shape the cache expects via RawLoad — see the comments
// on encodeNativeValue for the per-type contract.
func decodeNativeValue(vt apipersistence.ValueType, blob []byte) (any, error) {
	switch vt {
	case apipersistence.ValueTypeBytes:
		// String blobs are stored verbatim; copy so the caller doesn't
		// alias our scratch buffer (the reader may reuse it for the next
		// record).
		out := make([]byte, len(blob))
		copy(out, blob)
		return out, nil
	case apipersistence.ValueTypeList:
		return decodeStringSlice(blob)
	case apipersistence.ValueTypeHash:
		return decodeStringMap(blob)
	case apipersistence.ValueTypeSet:
		return decodeStringSet(blob)
	case apipersistence.ValueTypeSortedSet:
		return decodeSortedSet(blob)
	default:
		return nil, fmt.Errorf("v1snap: unknown ValueType %d", vt)
	}
}

func decodeStringSlice(blob []byte) ([]string, error) {
	count, n, err := readUvarint(blob)
	if err != nil {
		return nil, fmt.Errorf("list count: %w", err)
	}
	rest := blob[n:]
	out := make([]string, 0, count)
	for i := uint64(0); i < count; i++ {
		s, advance, err := readLengthPrefixedString(rest)
		if err != nil {
			return nil, fmt.Errorf("list item %d: %w", i, err)
		}
		out = append(out, s)
		rest = rest[advance:]
	}
	return out, nil
}

func decodeStringMap(blob []byte) (map[string]string, error) {
	count, n, err := readUvarint(blob)
	if err != nil {
		return nil, fmt.Errorf("hash count: %w", err)
	}
	rest := blob[n:]
	out := make(map[string]string, count)
	for i := uint64(0); i < count; i++ {
		k, advance, err := readLengthPrefixedString(rest)
		if err != nil {
			return nil, fmt.Errorf("hash key %d: %w", i, err)
		}
		rest = rest[advance:]
		v, advance, err := readLengthPrefixedString(rest)
		if err != nil {
			return nil, fmt.Errorf("hash val %d: %w", i, err)
		}
		out[k] = v
		rest = rest[advance:]
	}
	return out, nil
}

func decodeStringSet(blob []byte) (map[string]struct{}, error) {
	count, n, err := readUvarint(blob)
	if err != nil {
		return nil, fmt.Errorf("set count: %w", err)
	}
	rest := blob[n:]
	out := make(map[string]struct{}, count)
	for i := uint64(0); i < count; i++ {
		s, advance, err := readLengthPrefixedString(rest)
		if err != nil {
			return nil, fmt.Errorf("set member %d: %w", i, err)
		}
		out[s] = struct{}{}
		rest = rest[advance:]
	}
	return out, nil
}

func decodeSortedSet(blob []byte) (*cache.SortedSet, error) {
	count, n, err := readUvarint(blob)
	if err != nil {
		return nil, fmt.Errorf("zset count: %w", err)
	}
	rest := blob[n:]
	z := cache.NewSortedSet()
	for i := uint64(0); i < count; i++ {
		member, advance, err := readLengthPrefixedString(rest)
		if err != nil {
			return nil, fmt.Errorf("zset member %d: %w", i, err)
		}
		rest = rest[advance:]
		if len(rest) < 8 {
			return nil, fmt.Errorf("zset score %d: short read", i)
		}
		score := math.Float64frombits(binary.BigEndian.Uint64(rest[:8]))
		rest = rest[8:]
		z.Add(member, score)
	}
	return z, nil
}

// readUvarint pulls a varint off the front of b. Returns (value, bytes
// consumed, error). Errors when the buffer is empty or the varint is
// malformed (overlong / unterminated).
func readUvarint(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, fmt.Errorf("malformed varint")
	}
	return v, n, nil
}

// readLengthPrefixedString reads a varint-prefixed string. Returns the
// string and the total bytes consumed (varint + payload).
func readLengthPrefixedString(b []byte) (string, int, error) {
	length, n, err := readUvarint(b)
	if err != nil {
		return "", 0, err
	}
	if uint64(len(b)-n) < length {
		return "", 0, fmt.Errorf("short read: want %d got %d", length, len(b)-n)
	}
	return string(b[n : n+int(length)]), n + int(length), nil
}
