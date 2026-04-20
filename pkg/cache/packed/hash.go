package packed

import "gocache/pkg/cache"

// Hash layout: [count u32] ( [fieldLen u32] [field] [valueLen u32] [value] )*
//
// Iteration order is insertion order (matches listpack semantics and the
// existing DecodeHash native-map shape after promotion).

// HashNew returns an empty packed-hash buffer (4-byte zero header).
func HashNew() []byte {
	return make([]byte, HeaderBytes)
}

// HashLen returns the field count.
func HashLen(buf []byte) (int, error) {
	return readCount(buf)
}

// HashGet returns a zero-copy slice to the value for field. The returned
// slice must not be retained past the next mutation on buf.
func HashGet(buf []byte, field string) (value []byte, found bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, false, err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		f, afterField, err := readFrame(buf, pos)
		if err != nil {
			return nil, false, err
		}
		v, afterValue, err := readFrame(buf, afterField)
		if err != nil {
			return nil, false, err
		}
		if string(f) == field {
			return v, true, nil
		}
		pos = afterValue
	}
	return nil, false, nil
}

// HashContains reports whether field exists, without returning the value.
func HashContains(buf []byte, field string) (bool, error) {
	_, found, err := HashGet(buf, field)
	return found, err
}

// HashSet sets field=value. When the field exists and the new value matches
// the old value length, the update is in place and allocation-free.
// Otherwise the buffer is reallocated.
//
// Returns:
//   - newBuf:        possibly-reallocated buffer (caller must replace the
//                    entry with this).
//   - added:         true if this inserted a new field; false if it updated
//                    an existing one (HSET return-value semantics).
//   - shouldPromote: true when the post-op shape crosses either threshold
//                    (field count > maxEntries OR new value length >
//                    maxValueLen). Caller is responsible for promotion.
func HashSet(buf []byte, field, value string, maxEntries, maxValueLen int) (
	newBuf []byte, added bool, shouldPromote bool, err error,
) {
	if len(field) > cache.MaxCollectionItemLen || len(value) > cache.MaxCollectionItemLen {
		return buf, false, false, cache.ErrItemTooLarge
	}
	count, err := readCount(buf)
	if err != nil {
		return buf, false, false, err
	}

	pos := HeaderBytes
	for i := 0; i < count; i++ {
		f, afterField, err := readFrame(buf, pos)
		if err != nil {
			return buf, false, false, err
		}
		valueFrameStart := afterField
		v, afterValue, err := readFrame(buf, valueFrameStart)
		if err != nil {
			return buf, false, false, err
		}
		if string(f) == field {
			// Match: replace value in place when size matches, else splice.
			if len(v) == len(value) {
				copy(buf[valueFrameStart+LenPrefixBytes:afterValue], value)
				return buf, false, len(value) > maxValueLen, nil
			}
			need := len(buf) - (afterValue - valueFrameStart) + LenPrefixBytes + len(value)
			out := make([]byte, 0, need)
			out = append(out, buf[:valueFrameStart]...)
			out = appendFrameString(out, value)
			out = append(out, buf[afterValue:]...)
			return out, false, len(value) > maxValueLen, nil
		}
		pos = afterValue
	}

	// Not found: append new frame pair.
	if count+1 > cache.MaxCollectionItems {
		return buf, false, false, cache.ErrTooManyItems
	}
	need := len(buf) + 2*LenPrefixBytes + len(field) + len(value)
	out := make([]byte, len(buf), need)
	copy(out, buf)
	out = appendFrameString(out, field)
	out = appendFrameString(out, value)
	writeCount(out, count+1)
	shouldPromote = (count+1) > maxEntries || len(value) > maxValueLen
	return out, true, shouldPromote, nil
}

// HashDelete removes field from buf. Returns the (possibly reallocated)
// buffer and whether field was present.
func HashDelete(buf []byte, field string) (newBuf []byte, removed bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return buf, false, err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		entryStart := pos
		f, afterField, err := readFrame(buf, pos)
		if err != nil {
			return buf, false, err
		}
		_, afterValue, err := readFrame(buf, afterField)
		if err != nil {
			return buf, false, err
		}
		if string(f) == field {
			// Splice out [entryStart, afterValue).
			need := len(buf) - (afterValue - entryStart)
			out := make([]byte, 0, need)
			out = append(out, buf[:entryStart]...)
			out = append(out, buf[afterValue:]...)
			writeCount(out, count-1)
			return out, true, nil
		}
		pos = afterValue
	}
	return buf, false, nil
}

// HashIterate calls fn(field, value) for each pair in buf. Iteration stops
// when fn returns false. The slices passed to fn are zero-copy views into
// buf and must not be retained past the iteration.
func HashIterate(buf []byte, fn func(field, value []byte) bool) error {
	count, err := readCount(buf)
	if err != nil {
		return err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		f, afterField, err := readFrame(buf, pos)
		if err != nil {
			return err
		}
		v, afterValue, err := readFrame(buf, afterField)
		if err != nil {
			return err
		}
		if !fn(f, v) {
			return nil
		}
		pos = afterValue
	}
	return nil
}

// HashFields returns all field names as a newly-allocated []string. Used
// for HKEYS.
func HashFields(buf []byte) ([]string, error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, count)
	err = HashIterate(buf, func(field, _ []byte) bool {
		out = append(out, string(field))
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HashValues returns all values as a newly-allocated []string. Used for HVALS.
func HashValues(buf []byte) ([]string, error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, count)
	err = HashIterate(buf, func(_, value []byte) bool {
		out = append(out, string(value))
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HashToMap decodes buf into a native map[string]string. Used for
// Packed→Native promotion.
func HashToMap(buf []byte) (map[string]string, error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, count)
	err = HashIterate(buf, func(field, value []byte) bool {
		out[string(field)] = string(value)
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HashFromMap encodes m into a packed buffer. Used for persistence-layer
// round-trips where the on-disk form was stored as a Go map (pre-Phase 1
// snapshots).
func HashFromMap(m map[string]string) ([]byte, error) {
	if len(m) > cache.MaxCollectionItems {
		return nil, cache.ErrTooManyItems
	}
	size := HeaderBytes
	for k, v := range m {
		if len(k) > cache.MaxCollectionItemLen || len(v) > cache.MaxCollectionItemLen {
			return nil, cache.ErrItemTooLarge
		}
		size += 2*LenPrefixBytes + len(k) + len(v)
	}
	out := make([]byte, 0, size)
	out = append(out, HashNew()...)
	for k, v := range m {
		out = appendFrameString(out, k)
		out = appendFrameString(out, v)
	}
	writeCount(out, len(m))
	return out, nil
}
