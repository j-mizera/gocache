package snapshot

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	apipersistence "gocache/api/persistence"
)

// encodeNativeValue serialises the EncNative shape of a SnapshotEntry's
// Value into the v1 wire blob. Per ADR-0008, plugins receive only
// generic Go primitives at this boundary — no pkg/cache types — so the
// concrete type expected for each ValueType is:
//
//   - Bytes:     []byte (or string for legacy snapshots)
//   - List:      []string
//   - Hash:      map[string]string
//   - Set:       map[string]struct{}
//   - SortedSet: map[string]float64 (member → score)
//
// Encoded forms are documented in format.go. Native encodings for
// collections sort their entries to make the output byte-deterministic.
func encodeNativeValue(vt apipersistence.ValueType, value any) ([]byte, error) {
	switch vt {
	case apipersistence.ValueTypeBytes:
		return encodeBytes(value)
	case apipersistence.ValueTypeList:
		v, ok := value.([]string)
		if !ok {
			return nil, fmt.Errorf("snapshot: list native value: want []string, got %T", value)
		}
		return encodeStringSlice(v), nil
	case apipersistence.ValueTypeHash:
		v, ok := value.(map[string]string)
		if !ok {
			return nil, fmt.Errorf("snapshot: hash native value: want map[string]string, got %T", value)
		}
		return encodeStringMap(v), nil
	case apipersistence.ValueTypeSet:
		v, ok := value.(map[string]struct{})
		if !ok {
			return nil, fmt.Errorf("snapshot: set native value: want map[string]struct{}, got %T", value)
		}
		return encodeStringSet(v), nil
	case apipersistence.ValueTypeSortedSet:
		v, ok := value.(map[string]float64)
		if !ok {
			return nil, fmt.Errorf("snapshot: zset native value: want map[string]float64, got %T", value)
		}
		return encodeSortedSet(v), nil
	default:
		return nil, fmt.Errorf("snapshot: unknown ValueType %d", vt)
	}
}

// encodeBytes accepts the polymorphic []byte/string shape that legacy
// snapshots use for the String type. The wire output is raw bytes —
// length comes from the outer record framing.
func encodeBytes(value any) ([]byte, error) {
	switch v := value.(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("snapshot: string native value: want []byte or string, got %T", value)
	}
}

// encodeStringSlice writes: varint count + count×(varint len + bytes).
// Order is preserved — lists are ordered by definition.
func encodeStringSlice(s []string) []byte {
	out := make([]byte, 0, sliceEstimate(s))
	out = binary.AppendUvarint(out, uint64(len(s)))
	for _, item := range s {
		out = binary.AppendUvarint(out, uint64(len(item)))
		out = append(out, item...)
	}
	return out
}

// encodeStringMap writes: varint count + count×(varint key-len + key +
// varint val-len + val). Keys are lex-sorted for byte-determinism.
func encodeStringMap(m map[string]string) []byte {
	keys := sortedMapKeys(m)
	out := make([]byte, 0, mapEstimate(m))
	out = binary.AppendUvarint(out, uint64(len(keys)))
	for _, k := range keys {
		v := m[k]
		out = binary.AppendUvarint(out, uint64(len(k)))
		out = append(out, k...)
		out = binary.AppendUvarint(out, uint64(len(v)))
		out = append(out, v...)
	}
	return out
}

// encodeStringSet writes: varint count + count×(varint len + bytes).
// Members are lex-sorted for byte-determinism.
func encodeStringSet(s map[string]struct{}) []byte {
	members := sortedSetMembers(s)
	out := make([]byte, 0, setEstimate(s))
	out = binary.AppendUvarint(out, uint64(len(members)))
	for _, m := range members {
		out = binary.AppendUvarint(out, uint64(len(m)))
		out = append(out, m...)
	}
	return out
}

// encodeSortedSet writes: varint count + count×(varint member-len +
// member + 8B big-endian uint64 score-bits). Sorted by (score, member)
// for byte-deterministic output — same ordering the cache exposes via
// its sorted-set range methods.
func encodeSortedSet(members map[string]float64) []byte {
	type pair struct {
		member string
		score  float64
	}
	pairs := make([]pair, 0, len(members))
	for m, s := range members {
		pairs = append(pairs, pair{member: m, score: s})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].score != pairs[j].score {
			return pairs[i].score < pairs[j].score
		}
		return pairs[i].member < pairs[j].member
	})

	size := 1
	for _, p := range pairs {
		size += binary.MaxVarintLen64 + len(p.member) + 8
	}
	out := make([]byte, 0, size)
	out = binary.AppendUvarint(out, uint64(len(pairs)))
	scoreBuf := make([]byte, 8)
	for _, p := range pairs {
		out = binary.AppendUvarint(out, uint64(len(p.member)))
		out = append(out, p.member...)
		binary.BigEndian.PutUint64(scoreBuf, math.Float64bits(p.score))
		out = append(out, scoreBuf...)
	}
	return out
}

// sortedMapKeys returns the map's keys in lex-sorted order. Used to make
// hash encoding byte-deterministic.
func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedSetMembers returns set members in lex-sorted order.
func sortedSetMembers(s map[string]struct{}) []string {
	members := make([]string, 0, len(s))
	for m := range s {
		members = append(members, m)
	}
	sort.Strings(members)
	return members
}

// sliceEstimate, mapEstimate, setEstimate return rough byte upper-bounds
// for preallocating the output slice. Overestimating is fine — the
// final slice is sized to actual output. The alternative (precise
// pre-pass) costs an extra iteration without materially changing
// memory.
func sliceEstimate(s []string) int {
	n := binary.MaxVarintLen64
	for _, item := range s {
		n += binary.MaxVarintLen64 + len(item)
	}
	return n
}

func mapEstimate(m map[string]string) int {
	n := binary.MaxVarintLen64
	for k, v := range m {
		n += 2*binary.MaxVarintLen64 + len(k) + len(v)
	}
	return n
}

func setEstimate(s map[string]struct{}) int {
	n := binary.MaxVarintLen64
	for k := range s {
		n += binary.MaxVarintLen64 + len(k)
	}
	return n
}
