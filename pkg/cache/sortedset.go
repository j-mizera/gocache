package cache

import "sort"

// sortedSetMemberOverhead is the approximate per-member memory cost beyond
// the member string itself: 8 bytes for the float64 score + ~16 bytes of
// map bucket overhead.
const sortedSetMemberOverhead = 24

// SortedSet represents a sorted set with members and scores
type SortedSet struct {
	members map[string]float64 // member -> score
}

// NewSortedSet creates a new sorted set
func NewSortedSet() *SortedSet {
	return &SortedSet{
		members: make(map[string]float64),
	}
}

// Add adds or updates a member with a score
// Returns true if the member was newly added, false if updated
func (z *SortedSet) Add(member string, score float64) bool {
	_, exists := z.members[member]
	z.members[member] = score
	return !exists
}

// Remove removes a member
// Returns true if the member existed and was removed
func (z *SortedSet) Remove(member string) bool {
	_, exists := z.members[member]
	if exists {
		delete(z.members, member)
	}
	return exists
}

// Score returns the score of a member
// Returns score and true if member exists, 0 and false otherwise
func (z *SortedSet) Score(member string) (float64, bool) {
	score, exists := z.members[member]
	return score, exists
}

// Card returns the cardinality (number of members)
func (z *SortedSet) Card() int {
	return len(z.members)
}

// ScoredMember represents a member with its score
type ScoredMember struct {
	Member string
	Score  float64
}

// GetSortedMembers returns all members sorted by score (ascending)
func (z *SortedSet) GetSortedMembers() []ScoredMember {
	members := make([]ScoredMember, 0, len(z.members))
	for member, score := range z.members {
		members = append(members, ScoredMember{Member: member, Score: score})
	}

	sort.Slice(members, func(i, j int) bool {
		if members[i].Score != members[j].Score {
			return members[i].Score < members[j].Score
		}
		// If scores are equal, sort lexicographically by member
		return members[i].Member < members[j].Member
	})

	return members
}

// Rank returns the rank (0-based index) of a member when sorted by score
// Returns rank and true if member exists, -1 and false otherwise
func (z *SortedSet) Rank(member string) (int, bool) {
	if _, exists := z.members[member]; !exists {
		return -1, false
	}

	sorted := z.GetSortedMembers()
	for i, sm := range sorted {
		if sm.Member == member {
			return i, true
		}
	}
	return -1, false
}

// Range returns members in the given rank range [start, stop]
// Negative indices count from the end (-1 is last element)
func (z *SortedSet) Range(start, stop int) []ScoredMember {
	sorted := z.GetSortedMembers()
	length := len(sorted)

	if length == 0 {
		return []ScoredMember{}
	}

	// Handle negative indices
	if start < 0 {
		start = length + start
	}
	if stop < 0 {
		stop = length + stop
	}

	// Clamp to valid range
	if start < 0 {
		start = 0
	}
	if stop >= length {
		stop = length - 1
	}

	// Check if range is valid
	if start > stop || start >= length {
		return []ScoredMember{}
	}

	return sorted[start : stop+1]
}

// EstimateSize returns an approximate memory usage in bytes for this sorted set.
func (z *SortedSet) EstimateSize() int64 {
	var size int64
	for member := range z.members {
		size += int64(len(member)) + sortedSetMemberOverhead
	}
	return size
}

// Count returns the number of members with scores in the given range [min, max]
func (z *SortedSet) Count(min, max float64) int {
	count := 0
	for _, score := range z.members {
		if score >= min && score <= max {
			count++
		}
	}
	return count
}
