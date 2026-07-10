package benchsuite

import (
	"sort"
	"strings"
	"time"
)

// DurationRecorder records benchmark latency samples for percentile reporting.
type DurationRecorder struct {
	durations []time.Duration
}

// NewDurationRecorder creates a recorder with optional preallocated capacity.
func NewDurationRecorder(capacity int) *DurationRecorder {
	if capacity < 0 {
		capacity = 0
	}
	return &DurationRecorder{durations: make([]time.Duration, 0, capacity)}
}

// Record appends one latency sample.
func (recorder *DurationRecorder) Record(duration time.Duration) {
	recorder.durations = append(recorder.durations, duration)
}

// RecordSince appends the elapsed duration since startedAt.
func (recorder *DurationRecorder) RecordSince(startedAt time.Time) {
	recorder.Record(time.Since(startedAt))
}

// Reset clears recorded samples while retaining allocated storage.
func (recorder *DurationRecorder) Reset() {
	recorder.durations = recorder.durations[:0]
}

// Samples returns a copy of recorded duration samples.
func (recorder *DurationRecorder) Samples() []time.Duration {
	durationsCopy := make([]time.Duration, len(recorder.durations))
	copy(durationsCopy, recorder.durations)
	return durationsCopy
}

// PercentileSnapshot summarizes recorded latency samples.
type PercentileSnapshot struct {
	Count int
	Min   time.Duration
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	P999  time.Duration
	Max   time.Duration
}

// SampleSizeWarning returns a non-empty message when the sample count is
// insufficient for the percentiles reported in the snapshot. FR-006 requires
// at least 1,000 samples for p99 and at least 10,000 for p999. Single-run
// b.N output below these thresholds is an anecdote, not a measurement.
func (snapshot PercentileSnapshot) SampleSizeWarning() string {
	if snapshot.Count == 0 {
		return ""
	}
	var parts []string
	if snapshot.Count < 10000 {
		parts = append(parts, "p999 needs >=10000 samples")
	}
	if snapshot.Count < 1000 {
		parts = append(parts, "p99 needs >=1000 samples")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ")
}

// Snapshot computes nearest-rank percentiles from a sorted sample copy.
func (recorder *DurationRecorder) Snapshot() PercentileSnapshot {
	if len(recorder.durations) == 0 {
		return PercentileSnapshot{}
	}

	sortedDurations := recorder.Samples()
	sort.Slice(sortedDurations, func(leftIndex, rightIndex int) bool {
		return sortedDurations[leftIndex] < sortedDurations[rightIndex]
	})

	return PercentileSnapshot{
		Count: len(sortedDurations),
		Min:   sortedDurations[0],
		P50:   nearestRank(sortedDurations, 500),
		P95:   nearestRank(sortedDurations, 950),
		P99:   nearestRank(sortedDurations, 990),
		P999:  nearestRank(sortedDurations, 999),
		Max:   sortedDurations[len(sortedDurations)-1],
	}
}

func nearestRank(sortedDurations []time.Duration, percentilePermille int) time.Duration {
	if len(sortedDurations) == 0 {
		return 0
	}
	rank := (percentilePermille*len(sortedDurations) + 999) / 1000
	if rank < 1 {
		rank = 1
	}
	if rank > len(sortedDurations) {
		rank = len(sortedDurations)
	}
	return sortedDurations[rank-1]
}
