// Package packed implements byte-level in-place mutation ops for gocache's
// small-collection encoding (hash, list, set, sorted set).
//
// The encoding layout matches pkg/cache.EncodeX so promoting Packed→Native
// is a straight pkg/cache.DecodeX call. Mutations walk the byte buffer
// linearly (O(N) but N is bounded by per-type packed thresholds) and splice
// in place via copy/append — never decode-mutate-encode. This mirrors the
// small-side representation used by Valkey (listpack), Dragonfly, KeyDB.
//
// Invariants held by every op:
//
//   - buf has a 4-byte big-endian count header followed by type-specific
//     frames. An empty collection is the 4-byte zero header.
//   - Same-size replacements (same field length or same member length) are
//     O(1) and do not allocate.
//   - Size-changing mutations allocate at most one buffer.
//   - Zero unsafe.Pointer usage; this package stays GC-visible.
package packed

import (
	"encoding/binary"
	"math"

	"gocache/pkg/cache"
)

// Layout constants shared across all four collection encodings. The 4-byte
// count header lets XLen be O(1) without walking the buffer; the 4-byte
// length prefix on every frame makes forward iteration a simple index walk.
const (
	HeaderBytes    = 4 // [count u32 BE] at offset 0
	LenPrefixBytes = 4 // [len u32 BE] in front of every frame
	ScoreBytes     = 8 // [score f64 BE] prepended to every zset frame
)

// be aliases the canonical encoder used throughout the cache package so the
// packed layout and pkg/cache.EncodeX stay byte-identical.
var be = binary.BigEndian

// readCount returns the element count stored in buf's header. Returns
// ErrCorruptEncoding if buf is shorter than the header.
func readCount(buf []byte) (int, error) {
	if len(buf) < HeaderBytes {
		return 0, cache.ErrCorruptEncoding
	}
	return int(be.Uint32(buf[0:HeaderBytes])), nil
}

// writeCount rewrites the element count header in place. Caller guarantees
// buf has at least HeaderBytes.
func writeCount(buf []byte, count int) {
	be.PutUint32(buf[0:HeaderBytes], uint32(count))
}

// readFrame consumes one [len u32][bytes] frame starting at pos and returns
// a zero-copy slice into buf. The returned slice must not be retained past
// the next mutation of buf — callers that need a stable copy must allocate.
func readFrame(buf []byte, pos int) (frame []byte, next int, err error) {
	if pos+LenPrefixBytes > len(buf) {
		return nil, 0, cache.ErrCorruptEncoding
	}
	n := int(be.Uint32(buf[pos : pos+LenPrefixBytes]))
	pos += LenPrefixBytes
	if n > cache.MaxCollectionItemLen {
		return nil, 0, cache.ErrItemTooLarge
	}
	if pos+n > len(buf) {
		return nil, 0, cache.ErrCorruptEncoding
	}
	return buf[pos : pos+n], pos + n, nil
}

// readScore consumes an 8-byte score prefix and returns the float64 plus the
// position after the score.
func readScore(buf []byte, pos int) (score float64, next int, err error) {
	if pos+ScoreBytes > len(buf) {
		return 0, 0, cache.ErrCorruptEncoding
	}
	bits := be.Uint64(buf[pos : pos+ScoreBytes])
	return math.Float64frombits(bits), pos + ScoreBytes, nil
}

// appendLenU32 writes a 4-byte big-endian length prefix onto buf.
func appendLenU32(buf []byte, n int) []byte {
	var hdr [LenPrefixBytes]byte
	be.PutUint32(hdr[:], uint32(n))
	return append(buf, hdr[:]...)
}

// appendFrameString writes [len u32][frame bytes] onto buf. The `append(buf,
// s...)` form is the well-known zero-alloc string-to-bytes append idiom.
func appendFrameString(buf []byte, frame string) []byte {
	buf = appendLenU32(buf, len(frame))
	return append(buf, frame...)
}

// appendScore writes an 8-byte big-endian float64 score onto buf.
func appendScore(buf []byte, score float64) []byte {
	var hdr [ScoreBytes]byte
	be.PutUint64(hdr[:], math.Float64bits(score))
	return append(buf, hdr[:]...)
}
