package packed

import (
	"bytes"
	"sort"

	"gocache/pkg/cache"
)

// Set layout: [count u32] ( [memLen u32] [member] )*
//
// Members are stored in lexicographic ascending order so set-to-set ops
// (SINTER/SUNION/SDIFF) can walk in O(n+m) instead of O(n·m) scan. Order
// also means SISMEMBER can binary-search the members via a two-pass scan.

// SetNew returns an empty packed-set buffer.
func SetNew() []byte {
	return make([]byte, HeaderBytes)
}

// SetLen returns the member count.
func SetLen(buf []byte) (int, error) {
	return readCount(buf)
}

// setOffsets returns a sorted list of (frame start, frame end) for every
// member. Allocates once — the caller decides what to do with the offsets.
// This is the workhorse shared by sorted-insertion ops.
func setOffsets(buf []byte) (starts []int, ends []int, err error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, nil, err
	}
	starts = make([]int, count)
	ends = make([]int, count)
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		starts[i] = pos
		_, next, err := readFrame(buf, pos)
		if err != nil {
			return nil, nil, err
		}
		ends[i] = next
		pos = next
	}
	return starts, ends, nil
}

// setFrameBytes extracts the member portion (skipping the length prefix)
// from a frame in buf.
func setFrameBytes(buf []byte, start int) []byte {
	n := int(be.Uint32(buf[start : start+LenPrefixBytes]))
	return buf[start+LenPrefixBytes : start+LenPrefixBytes+n]
}

// SetContains reports whether member exists. Binary-searches the sorted
// layout.
func SetContains(buf []byte, member string) (bool, error) {
	starts, _, err := setOffsets(buf)
	if err != nil {
		return false, err
	}
	// Linear scan for now — bounded by maxEntries ≤ 512. If benchmarking
	// shows this matters we'll binary-search via starts[] indexing.
	for _, s := range starts {
		if string(setFrameBytes(buf, s)) == member {
			return true, nil
		}
	}
	return false, nil
}

// SetAdd inserts member in sorted order. Returns the new buffer,
// added=false if it was already present, shouldPromote=true when the result
// crosses either threshold.
func SetAdd(buf []byte, member string, maxEntries, maxValueLen int) (
	newBuf []byte, added bool, shouldPromote bool, err error,
) {
	if len(member) > cache.MaxCollectionItemLen {
		return buf, false, false, cache.ErrItemTooLarge
	}
	count, err := readCount(buf)
	if err != nil {
		return buf, false, false, err
	}
	if count+1 > cache.MaxCollectionItems {
		return buf, false, false, cache.ErrTooManyItems
	}
	// Walk to find insertion position.
	target := []byte(member)
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		frameStart := pos
		frame, next, err := readFrame(buf, pos)
		if err != nil {
			return buf, false, false, err
		}
		cmp := bytes.Compare(frame, target)
		if cmp == 0 {
			return buf, false, false, nil // already present
		}
		if cmp > 0 {
			// Insert before frameStart.
			need := len(buf) + LenPrefixBytes + len(member)
			out := make([]byte, 0, need)
			out = append(out, buf[:frameStart]...)
			out = appendFrameString(out, member)
			out = append(out, buf[frameStart:]...)
			writeCount(out, count+1)
			shouldPromote = (count+1) > maxEntries || len(member) > maxValueLen
			return out, true, shouldPromote, nil
		}
		pos = next
	}
	// Append at end (member is the new max).
	need := len(buf) + LenPrefixBytes + len(member)
	out := make([]byte, len(buf), need)
	copy(out, buf)
	out = appendFrameString(out, member)
	writeCount(out, count+1)
	shouldPromote = (count+1) > maxEntries || len(member) > maxValueLen
	return out, true, shouldPromote, nil
}

// SetRemove removes member. Returns the new buffer and removed status.
func SetRemove(buf []byte, member string) (newBuf []byte, removed bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return buf, false, err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		frameStart := pos
		frame, next, err := readFrame(buf, pos)
		if err != nil {
			return buf, false, err
		}
		if string(frame) == member {
			need := len(buf) - (next - frameStart)
			out := make([]byte, 0, need)
			out = append(out, buf[:frameStart]...)
			out = append(out, buf[next:]...)
			writeCount(out, count-1)
			return out, true, nil
		}
		pos = next
	}
	return buf, false, nil
}

// SetIterate calls fn(member) for each member in sorted order. Iteration
// stops when fn returns false. Zero-copy into buf.
func SetIterate(buf []byte, fn func(member []byte) bool) error {
	count, err := readCount(buf)
	if err != nil {
		return err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		frame, next, err := readFrame(buf, pos)
		if err != nil {
			return err
		}
		if !fn(frame) {
			return nil
		}
		pos = next
	}
	return nil
}

// SetMembers returns all members as a newly-allocated []string, sorted.
func SetMembers(buf []byte) ([]string, error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, count)
	err = SetIterate(buf, func(m []byte) bool {
		out = append(out, string(m))
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetToMap returns a map[string]struct{} for Packed→Native promotion.
func SetToMap(buf []byte) (map[string]struct{}, error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, count)
	err = SetIterate(buf, func(m []byte) bool {
		out[string(m)] = struct{}{}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetFromMap encodes a native set into a packed buffer. Members are sorted
// before writing to match the layout invariant.
func SetFromMap(m map[string]struct{}) ([]byte, error) {
	if len(m) > cache.MaxCollectionItems {
		return nil, cache.ErrTooManyItems
	}
	members := make([]string, 0, len(m))
	for k := range m {
		if len(k) > cache.MaxCollectionItemLen {
			return nil, cache.ErrItemTooLarge
		}
		members = append(members, k)
	}
	sort.Strings(members)
	size := HeaderBytes
	for _, s := range members {
		size += LenPrefixBytes + len(s)
	}
	out := make([]byte, HeaderBytes, size)
	writeCount(out, len(members))
	for _, s := range members {
		out = appendFrameString(out, s)
	}
	return out, nil
}
