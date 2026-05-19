package packed

import (
	"github.com/gammazero/deque"

	"gocache/pkg/cache"
)

// List layout: [count u32] ( [len u32] [item] )*
//
// Items are stored left-to-right (index 0 = leftmost). LPUSH prepends,
// RPUSH appends. Pops at either end rewrite the count header and splice
// off the end frame.
//
// Promotion signal is based on total byte size rather than count — Valkey's
// list-max-listpack-size default is bytes, not entries, because list items
// are typically longer than hash field names.

// ListNew returns an empty packed-list buffer.
func ListNew() []byte {
	return make([]byte, HeaderBytes)
}

// ListLen returns the item count.
func ListLen(buf []byte) (int, error) {
	return readCount(buf)
}

// ListAppendRight appends items onto the right (RPUSH). Returns the new
// buffer and shouldPromote when len(newBuf) > maxBytes.
func ListAppendRight(buf []byte, items []string, maxBytes int) (
	newBuf []byte, shouldPromote bool, err error,
) {
	for _, it := range items {
		if len(it) > cache.MaxCollectionItemLen {
			return buf, false, cache.ErrItemTooLarge
		}
	}
	count, err := readCount(buf)
	if err != nil {
		return buf, false, err
	}
	if count+len(items) > cache.MaxCollectionItems {
		return buf, false, cache.ErrTooManyItems
	}
	size := len(buf)
	for _, it := range items {
		size += LenPrefixBytes + len(it)
	}
	out := make([]byte, len(buf), size)
	copy(out, buf)
	for _, it := range items {
		out = appendFrameString(out, it)
	}
	writeCount(out, count+len(items))
	return out, len(out) > maxBytes, nil
}

// ListAppendLeft prepends items onto the left (LPUSH). Semantically
// equivalent to `LPUSH k a b c` → list becomes [c, b, a, ...] because Redis
// pushes each arg sequentially from the left.
//
// items are pushed in order: items[0] ends up furthest right (least
// recently pushed) and items[len-1] ends up leftmost.
func ListAppendLeft(buf []byte, items []string, maxBytes int) (
	newBuf []byte, shouldPromote bool, err error,
) {
	for _, it := range items {
		if len(it) > cache.MaxCollectionItemLen {
			return buf, false, cache.ErrItemTooLarge
		}
	}
	count, err := readCount(buf)
	if err != nil {
		return buf, false, err
	}
	if count+len(items) > cache.MaxCollectionItems {
		return buf, false, cache.ErrTooManyItems
	}
	prefixSize := 0
	for _, it := range items {
		prefixSize += LenPrefixBytes + len(it)
	}
	size := len(buf) + prefixSize
	out := make([]byte, HeaderBytes, size)
	writeCount(out, count+len(items))
	// Items are pushed left-to-right from the iteration order — the last
	// element of items becomes the leftmost list item. Build the new frame
	// block in reverse so items[len-1] is written first.
	for i := len(items) - 1; i >= 0; i-- {
		out = appendFrameString(out, items[i])
	}
	// Copy the remainder of the old buffer past the header.
	out = append(out, buf[HeaderBytes:]...)
	return out, len(out) > maxBytes, nil
}

// ListPopLeft removes and returns the leftmost item.
func ListPopLeft(buf []byte) (newBuf []byte, item []byte, ok bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return buf, nil, false, err
	}
	if count == 0 {
		return buf, nil, false, nil
	}
	pos := HeaderBytes
	frame, afterFrame, err := readFrame(buf, pos)
	if err != nil {
		return buf, nil, false, err
	}
	// Callers mutate the returned item before looking at the buffer again
	// (LPOP returns the bytes then dispatch continues), but to be safe
	// against accidental retention we copy into a fresh []byte.
	popped := make([]byte, len(frame))
	copy(popped, frame)
	need := HeaderBytes + (len(buf) - afterFrame)
	out := make([]byte, 0, need)
	out = append(out, make([]byte, HeaderBytes)...)
	out = append(out, buf[afterFrame:]...)
	writeCount(out, count-1)
	return out, popped, true, nil
}

// ListPopRight removes and returns the rightmost item. O(N) because we have
// to walk to find the last frame — acceptable for packed lists (bounded by
// maxBytes so N ≤ a few hundred items).
func ListPopRight(buf []byte) (newBuf []byte, item []byte, ok bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return buf, nil, false, err
	}
	if count == 0 {
		return buf, nil, false, nil
	}
	pos := HeaderBytes
	lastStart := pos
	for i := 0; i < count; i++ {
		lastStart = pos
		_, next, err := readFrame(buf, pos)
		if err != nil {
			return buf, nil, false, err
		}
		pos = next
	}
	// buf[lastStart:pos] is the final frame.
	frame, _, err := readFrame(buf, lastStart)
	if err != nil {
		return buf, nil, false, err
	}
	popped := make([]byte, len(frame))
	copy(popped, frame)
	need := lastStart
	out := make([]byte, 0, need)
	out = append(out, buf[:lastStart]...)
	writeCount(out, count-1)
	return out, popped, true, nil
}

// ListIndex returns the item at the given index (negative = from the right).
// The returned slice is zero-copy into buf.
func ListIndex(buf []byte, index int) (item []byte, found bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, false, err
	}
	if index < 0 {
		index += count
	}
	if index < 0 || index >= count {
		return nil, false, nil
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		frame, next, err := readFrame(buf, pos)
		if err != nil {
			return nil, false, err
		}
		if i == index {
			return frame, true, nil
		}
		pos = next
	}
	return nil, false, nil
}

// ListRange returns items[start:stop] (inclusive, matching LRANGE).
// Negative indices count from the end. Returns an empty slice if the range
// is empty after normalization.
func ListRange(buf []byte, start, stop int) ([]string, error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	if start < 0 {
		start += count
	}
	if stop < 0 {
		stop += count
	}
	if start < 0 {
		start = 0
	}
	if stop >= count {
		stop = count - 1
	}
	if start > stop || start >= count {
		return nil, nil
	}
	out := make([]string, 0, stop-start+1)
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		frame, next, err := readFrame(buf, pos)
		if err != nil {
			return nil, err
		}
		if i >= start && i <= stop {
			out = append(out, string(frame))
		}
		if i > stop {
			break
		}
		pos = next
	}
	return out, nil
}

// ListIterate calls fn(index, item) for each item in buf. Iteration stops
// when fn returns false.
func ListIterate(buf []byte, fn func(index int, item []byte) bool) error {
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
		if !fn(i, frame) {
			return nil
		}
		pos = next
	}
	return nil
}

// ListToSlice decodes buf into []string. Used for Packed→Native promotion.
func ListToSlice(buf []byte) ([]string, error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, count)
	err = ListIterate(buf, func(_ int, item []byte) bool {
		out = append(out, string(item))
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListToDeque decodes buf into *deque.Deque[string]. Used for Packed→Native promotion.
func ListToDeque(buf []byte) (*deque.Deque[string], error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	dq := new(deque.Deque[string])
	dq.Grow(count)
	err = ListIterate(buf, func(_ int, item []byte) bool {
		dq.PushBack(string(item))
		return true
	})
	if err != nil {
		return nil, err
	}
	return dq, nil
}

// ListFromSlice encodes items into a packed buffer. Used for native→packed
// conversion when snapshot-loading older on-disk formats.
func ListFromSlice(items []string) ([]byte, error) {
	if len(items) > cache.MaxCollectionItems {
		return nil, cache.ErrTooManyItems
	}
	size := HeaderBytes
	for _, it := range items {
		if len(it) > cache.MaxCollectionItemLen {
			return nil, cache.ErrItemTooLarge
		}
		size += LenPrefixBytes + len(it)
	}
	out := make([]byte, HeaderBytes, size)
	writeCount(out, len(items))
	for _, it := range items {
		out = appendFrameString(out, it)
	}
	return out, nil
}

// ListRemove removes up to |count| occurrences of needle. count > 0 removes
// from head to tail, count < 0 from tail to head, count == 0 removes all.
// Returns the new buffer and the number of items actually removed.
func ListRemove(buf []byte, needle string, count int) (newBuf []byte, removed int, err error) {
	total, err := readCount(buf)
	if err != nil {
		return buf, 0, err
	}
	if total == 0 {
		return buf, 0, nil
	}

	// First pass: collect frame boundaries.
	type frameSpan struct{ start, end int }
	frames := make([]frameSpan, 0, total)
	matches := make([]int, 0) // indices into frames that match
	pos := HeaderBytes
	for i := 0; i < total; i++ {
		start := pos
		frame, next, err := readFrame(buf, pos)
		if err != nil {
			return buf, 0, err
		}
		frames = append(frames, frameSpan{start: start, end: next})
		if string(frame) == needle {
			matches = append(matches, i)
		}
		pos = next
	}
	if len(matches) == 0 {
		return buf, 0, nil
	}

	// Pick the indices to drop.
	var drop []int
	switch {
	case count > 0:
		if count > len(matches) {
			count = len(matches)
		}
		drop = matches[:count]
	case count < 0:
		n := -count
		if n > len(matches) {
			n = len(matches)
		}
		drop = matches[len(matches)-n:]
	default:
		drop = matches
	}
	dropSet := make(map[int]struct{}, len(drop))
	for _, idx := range drop {
		dropSet[idx] = struct{}{}
	}

	// Rebuild buffer without dropped frames.
	out := make([]byte, 0, len(buf))
	out = append(out, make([]byte, HeaderBytes)...)
	newCount := 0
	for i, f := range frames {
		if _, dropped := dropSet[i]; dropped {
			continue
		}
		out = append(out, buf[f.start:f.end]...)
		newCount++
	}
	writeCount(out, newCount)
	return out, len(drop), nil
}

// ListSet replaces the item at index. Returns the new buffer and whether
// the index was valid.
func ListSet(buf []byte, index int, value string) (newBuf []byte, ok bool, err error) {
	if len(value) > cache.MaxCollectionItemLen {
		return buf, false, cache.ErrItemTooLarge
	}
	count, err := readCount(buf)
	if err != nil {
		return buf, false, err
	}
	if index < 0 {
		index += count
	}
	if index < 0 || index >= count {
		return buf, false, nil
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		frameStart := pos
		frame, next, err := readFrame(buf, pos)
		if err != nil {
			return buf, false, err
		}
		if i == index {
			if len(frame) == len(value) {
				copy(buf[frameStart+LenPrefixBytes:next], value)
				return buf, true, nil
			}
			need := len(buf) - (next - frameStart) + LenPrefixBytes + len(value)
			out := make([]byte, 0, need)
			out = append(out, buf[:frameStart]...)
			out = appendFrameString(out, value)
			out = append(out, buf[next:]...)
			return out, true, nil
		}
		pos = next
	}
	return buf, false, nil
}

// ListTrim retains items[start:stop] (inclusive). Returns the new buffer.
// Matches LTRIM semantics: out-of-range indices clamp to [0, count-1].
func ListTrim(buf []byte, start, stop int) (newBuf []byte, err error) {
	count, err := readCount(buf)
	if err != nil {
		return buf, err
	}
	if count == 0 {
		return buf, nil
	}
	if start < 0 {
		start += count
	}
	if stop < 0 {
		stop += count
	}
	if start < 0 {
		start = 0
	}
	if stop >= count {
		stop = count - 1
	}
	if start > stop || start >= count {
		return ListNew(), nil
	}

	pos := HeaderBytes
	var firstKept, lastKept int
	for i := 0; i < count; i++ {
		frameStart := pos
		_, next, err := readFrame(buf, pos)
		if err != nil {
			return buf, err
		}
		if i == start {
			firstKept = frameStart
		}
		if i == stop {
			lastKept = next
		}
		pos = next
	}
	newCount := stop - start + 1
	out := make([]byte, 0, HeaderBytes+(lastKept-firstKept))
	out = append(out, make([]byte, HeaderBytes)...)
	out = append(out, buf[firstKept:lastKept]...)
	writeCount(out, newCount)
	return out, nil
}
