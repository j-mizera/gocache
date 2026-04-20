package packed

import (
	"bytes"
	"math"
	"sort"

	"gocache/pkg/cache"
)

// ZSet layout: [count u32] ( [score f64] [memLen u32] [member] )*
//
// Stored sorted by (score ascending, member ascending) so ZRANGE is a
// linear walk, ZRANK is a linear scan (bounded by zset_max_packed_entries
// ≤ 128), and ZRANGEBYSCORE can binary-search then walk.

// ZSetNew returns an empty packed-zset buffer.
func ZSetNew() []byte {
	return make([]byte, HeaderBytes)
}

// ZSetLen returns the member count.
func ZSetLen(buf []byte) (int, error) {
	return readCount(buf)
}

// ZSetScoreOf returns the score for member and whether it exists.
func ZSetScoreOf(buf []byte, member string) (score float64, found bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return 0, false, err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		s, afterScore, err := readScore(buf, pos)
		if err != nil {
			return 0, false, err
		}
		m, next, err := readFrame(buf, afterScore)
		if err != nil {
			return 0, false, err
		}
		if string(m) == member {
			return s, true, nil
		}
		pos = next
	}
	return 0, false, nil
}

// zsetLessEntry compares (scoreA, memberA) < (scoreB, memberB) for sort order.
func zsetLessEntry(scoreA float64, memberA []byte, scoreB float64, memberB []byte) bool {
	if scoreA != scoreB {
		return scoreA < scoreB
	}
	return bytes.Compare(memberA, memberB) < 0
}

// ZSetAdd inserts or updates member=score. Returns the new buffer,
// added=true if this added a new member (false if it updated a score),
// scoreChanged=true if an existing member's score changed, shouldPromote.
func ZSetAdd(buf []byte, member string, score float64, maxEntries, maxValueLen int) (
	newBuf []byte, added bool, scoreChanged bool, shouldPromote bool, err error,
) {
	if len(member) > cache.MaxCollectionItemLen {
		return buf, false, false, false, cache.ErrItemTooLarge
	}
	if math.IsNaN(score) {
		// Redis rejects NaN scores — match that behaviour.
		return buf, false, false, false, cache.ErrCorruptEncoding
	}
	count, err := readCount(buf)
	if err != nil {
		return buf, false, false, false, err
	}

	// First pass: find if member already exists.
	target := []byte(member)
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		entryStart := pos
		s, afterScore, err := readScore(buf, pos)
		if err != nil {
			return buf, false, false, false, err
		}
		m, next, err := readFrame(buf, afterScore)
		if err != nil {
			return buf, false, false, false, err
		}
		if bytes.Equal(m, target) {
			if s == score {
				return buf, false, false, false, nil // no-op
			}
			// Remove existing entry, re-insert at new sorted position.
			out := make([]byte, 0, len(buf))
			out = append(out, buf[:entryStart]...)
			out = append(out, buf[next:]...)
			writeCount(out, count-1)
			// Insert new frame sorted.
			out = zsetInsertSorted(out, count-1, score, member)
			shouldPromote = len(member) > maxValueLen
			return out, false, true, shouldPromote, nil
		}
		pos = next
	}

	// Not present: insert sorted.
	if count+1 > cache.MaxCollectionItems {
		return buf, false, false, false, cache.ErrTooManyItems
	}
	out := zsetInsertSorted(buf, count, score, member)
	shouldPromote = (count+1) > maxEntries || len(member) > maxValueLen
	return out, true, false, shouldPromote, nil
}

// zsetInsertSorted inserts [score, member] into buf at the correct
// (score-asc, member-asc) position. count is the pre-insertion count;
// buf's header is rewritten to count+1.
func zsetInsertSorted(buf []byte, count int, score float64, member string) []byte {
	target := []byte(member)
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		entryStart := pos
		s, afterScore, err := readScore(buf, pos)
		if err != nil {
			// corrupt — just append; caller's previous readCount would have
			// caught this. Fall through defensively.
			break
		}
		m, next, err := readFrame(buf, afterScore)
		if err != nil {
			break
		}
		if zsetLessEntry(score, target, s, m) {
			need := len(buf) + ScoreBytes + LenPrefixBytes + len(member)
			out := make([]byte, 0, need)
			out = append(out, buf[:entryStart]...)
			out = appendScore(out, score)
			out = appendFrameString(out, member)
			out = append(out, buf[entryStart:]...)
			writeCount(out, count+1)
			return out
		}
		pos = next
	}
	// Append at end.
	need := len(buf) + ScoreBytes + LenPrefixBytes + len(member)
	out := make([]byte, len(buf), need)
	copy(out, buf)
	out = appendScore(out, score)
	out = appendFrameString(out, member)
	writeCount(out, count+1)
	return out
}

// ZSetRemove removes member. Returns the new buffer and removed status.
func ZSetRemove(buf []byte, member string) (newBuf []byte, removed bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return buf, false, err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		entryStart := pos
		_, afterScore, err := readScore(buf, pos)
		if err != nil {
			return buf, false, err
		}
		m, next, err := readFrame(buf, afterScore)
		if err != nil {
			return buf, false, err
		}
		if string(m) == member {
			need := len(buf) - (next - entryStart)
			out := make([]byte, 0, need)
			out = append(out, buf[:entryStart]...)
			out = append(out, buf[next:]...)
			writeCount(out, count-1)
			return out, true, nil
		}
		pos = next
	}
	return buf, false, nil
}

// ZSetRank returns the 0-based rank of member (ascending by score then
// member). withScore returns the score alongside.
func ZSetRank(buf []byte, member string) (rank int, score float64, found bool, err error) {
	count, err := readCount(buf)
	if err != nil {
		return 0, 0, false, err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		s, afterScore, err := readScore(buf, pos)
		if err != nil {
			return 0, 0, false, err
		}
		m, next, err := readFrame(buf, afterScore)
		if err != nil {
			return 0, 0, false, err
		}
		if string(m) == member {
			return i, s, true, nil
		}
		pos = next
	}
	return 0, 0, false, nil
}

// ZSetRangeByIndex returns scored members in the index range [start, stop]
// (inclusive). Negative indices count from the end (ZRANGE semantics).
func ZSetRangeByIndex(buf []byte, start, stop int) ([]cache.ScoredMember, error) {
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
	out := make([]cache.ScoredMember, 0, stop-start+1)
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		s, afterScore, err := readScore(buf, pos)
		if err != nil {
			return nil, err
		}
		m, next, err := readFrame(buf, afterScore)
		if err != nil {
			return nil, err
		}
		if i >= start && i <= stop {
			out = append(out, cache.ScoredMember{Score: s, Member: string(m)})
		}
		if i > stop {
			break
		}
		pos = next
	}
	return out, nil
}

// ZSetRangeByScore returns scored members whose score is within
// [min, max], inclusive. Matches ZRANGEBYSCORE default behaviour.
func ZSetRangeByScore(buf []byte, min, max float64) ([]cache.ScoredMember, error) {
	count, err := readCount(buf)
	if err != nil {
		return nil, err
	}
	if count == 0 || min > max {
		return nil, nil
	}
	out := make([]cache.ScoredMember, 0)
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		s, afterScore, err := readScore(buf, pos)
		if err != nil {
			return nil, err
		}
		m, next, err := readFrame(buf, afterScore)
		if err != nil {
			return nil, err
		}
		if s >= min && s <= max {
			out = append(out, cache.ScoredMember{Score: s, Member: string(m)})
		}
		if s > max {
			break
		}
		pos = next
	}
	return out, nil
}

// ZSetCountByScore returns the count of members whose score is within
// [min, max], inclusive.
func ZSetCountByScore(buf []byte, min, max float64) (int, error) {
	count, err := readCount(buf)
	if err != nil {
		return 0, err
	}
	if count == 0 || min > max {
		return 0, nil
	}
	n := 0
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		s, afterScore, err := readScore(buf, pos)
		if err != nil {
			return 0, err
		}
		_, next, err := readFrame(buf, afterScore)
		if err != nil {
			return 0, err
		}
		if s >= min && s <= max {
			n++
		}
		if s > max {
			break
		}
		pos = next
	}
	return n, nil
}

// ZSetIterate calls fn(score, member) for each entry in sorted order.
func ZSetIterate(buf []byte, fn func(score float64, member []byte) bool) error {
	count, err := readCount(buf)
	if err != nil {
		return err
	}
	pos := HeaderBytes
	for i := 0; i < count; i++ {
		s, afterScore, err := readScore(buf, pos)
		if err != nil {
			return err
		}
		m, next, err := readFrame(buf, afterScore)
		if err != nil {
			return err
		}
		if !fn(s, m) {
			return nil
		}
		pos = next
	}
	return nil
}

// ZSetToNative decodes buf into a *SortedSet for Packed→Native promotion.
func ZSetToNative(buf []byte) (*cache.SortedSet, error) {
	z := cache.NewSortedSet()
	err := ZSetIterate(buf, func(score float64, member []byte) bool {
		z.Add(string(member), score)
		return true
	})
	if err != nil {
		return nil, err
	}
	return z, nil
}

// ZSetFromPairs encodes sorted (score-asc, member-asc) pairs into a packed
// buffer. The caller guarantees pairs are already in sorted order — this
// is the contract with the cache package's zset layout.
func ZSetFromPairs(pairs []cache.ScoredMember) ([]byte, error) {
	if len(pairs) > cache.MaxCollectionItems {
		return nil, cache.ErrTooManyItems
	}
	size := HeaderBytes
	for _, p := range pairs {
		if len(p.Member) > cache.MaxCollectionItemLen {
			return nil, cache.ErrItemTooLarge
		}
		size += ScoreBytes + LenPrefixBytes + len(p.Member)
	}
	out := make([]byte, HeaderBytes, size)
	writeCount(out, len(pairs))
	for _, p := range pairs {
		out = appendScore(out, p.Score)
		out = appendFrameString(out, p.Member)
	}
	return out, nil
}

// ZSetFromNative sorts and encodes a *SortedSet into a packed buffer. Used
// for native→packed conversion when snapshot-loading older on-disk formats.
func ZSetFromNative(z *cache.SortedSet) ([]byte, error) {
	if z == nil {
		return ZSetNew(), nil
	}
	pairs := z.GetSortedMembers()
	// GetSortedMembers returns score-asc, member-asc already — but be
	// defensive in case contract drifts.
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].Score != pairs[j].Score {
			return pairs[i].Score < pairs[j].Score
		}
		return pairs[i].Member < pairs[j].Member
	})
	return ZSetFromPairs(pairs)
}
