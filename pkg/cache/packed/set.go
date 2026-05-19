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
// layout via the offset index.
func SetContains(buf []byte, member string) (bool, error) {
	starts, _, err := setOffsets(buf)
	if err != nil {
		return false, err
	}
	target := []byte(member)
	idx := sort.Search(len(starts), func(i int) bool {
		return bytes.Compare(setFrameBytes(buf, starts[i]), target) >= 0
	})
	if idx < len(starts) && bytes.Equal(setFrameBytes(buf, starts[idx]), target) {
		return true, nil
	}
	return false, nil
}

// SetAdd inserts member in sorted order. Returns the new buffer,
// added=false if it was already present, shouldPromote=true when the result
// crosses either threshold. Uses binary search over the offset index.
func SetAdd(buf []byte, member string, maxEntries, maxValueLen int) (
	newBuf []byte, added bool, shouldPromote bool, err error,
) {
	if len(member) > cache.MaxCollectionItemLen {
		return buf, false, false, cache.ErrItemTooLarge
	}
	starts, _, err := setOffsets(buf)
	if err != nil {
		return buf, false, false, err
	}
	count := len(starts)
	if count+1 > cache.MaxCollectionItems {
		return buf, false, false, cache.ErrTooManyItems
	}
	target := []byte(member)
	idx := sort.Search(count, func(i int) bool {
		return bytes.Compare(setFrameBytes(buf, starts[i]), target) >= 0
	})
	if idx < count && bytes.Equal(setFrameBytes(buf, starts[idx]), target) {
		return buf, false, false, nil
	}
	shouldPromote = (count+1) > maxEntries || len(member) > maxValueLen
	need := len(buf) + LenPrefixBytes + len(member)
	if idx < count {
		insertAt := starts[idx]
		out := make([]byte, 0, need)
		out = append(out, buf[:insertAt]...)
		out = appendFrameString(out, member)
		out = append(out, buf[insertAt:]...)
		writeCount(out, count+1)
		return out, true, shouldPromote, nil
	}
	out := make([]byte, len(buf), need)
	copy(out, buf)
	out = appendFrameString(out, member)
	writeCount(out, count+1)
	return out, true, shouldPromote, nil
}

// SetAddBatch inserts multiple members in a single pass. Sorts and
// deduplicates members, then sort-merges with the existing sorted layout.
// O(N + K log K) instead of O(N × K) for K individual SetAdd calls.
func SetAddBatch(buf []byte, members []string, maxEntries, maxValueLen int) (
	newBuf []byte, added int, shouldPromote bool, err error,
) {
	if len(members) == 1 {
		nb, ok, sp, err := SetAdd(buf, members[0], maxEntries, maxValueLen)
		if ok {
			return nb, 1, sp, err
		}
		return nb, 0, sp, err
	}
	for _, m := range members {
		if len(m) > cache.MaxCollectionItemLen {
			return buf, 0, false, cache.ErrItemTooLarge
		}
	}
	starts, ends, err := setOffsets(buf)
	if err != nil {
		return buf, 0, false, err
	}
	existCount := len(starts)

	sorted := make([]string, len(members))
	copy(sorted, members)
	sort.Strings(sorted)
	deduped := sorted[:0]
	for i, m := range sorted {
		if i == 0 || m != sorted[i-1] {
			deduped = append(deduped, m)
		}
	}

	capEst := len(buf)
	for _, m := range deduped {
		capEst += LenPrefixBytes + len(m)
	}

	out := make([]byte, HeaderBytes, capEst)
	ei, ni := 0, 0
	for ei < existCount && ni < len(deduped) {
		existing := setFrameBytes(buf, starts[ei])
		cmp := bytes.Compare(existing, []byte(deduped[ni]))
		switch {
		case cmp < 0:
			out = append(out, buf[starts[ei]:ends[ei]]...)
			ei++
		case cmp == 0:
			out = append(out, buf[starts[ei]:ends[ei]]...)
			ei++
			ni++
		default:
			out = appendFrameString(out, deduped[ni])
			added++
			ni++
		}
	}
	for ; ei < existCount; ei++ {
		out = append(out, buf[starts[ei]:ends[ei]]...)
	}
	for ; ni < len(deduped); ni++ {
		out = appendFrameString(out, deduped[ni])
		added++
	}
	if added == 0 {
		return buf, 0, false, nil
	}

	newCount := existCount + added
	if newCount > cache.MaxCollectionItems {
		return buf, 0, false, cache.ErrTooManyItems
	}
	writeCount(out, newCount)

	shouldPromote = newCount > maxEntries
	if !shouldPromote {
		for _, m := range deduped {
			if len(m) > maxValueLen {
				shouldPromote = true
				break
			}
		}
	}
	return out, added, shouldPromote, nil
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
