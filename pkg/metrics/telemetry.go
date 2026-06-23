package metrics

import (
	"fmt"
	"strconv"

	commonobs "gocache/commons/observability"
)

// TelemetryTopic is the server-query topic for operation-tracker telemetry snapshots.
const TelemetryTopic = "metrics.telemetry"

// OperationTrackerTelemetrySource is the narrow operation-tracker surface needed
// to publish telemetry pressure through server-query snapshots.
type OperationTrackerTelemetrySource interface {
	SkippedOperations() uint64
	DroppedRecords() uint64
	DroppedCompletedOperations() uint64
	InvalidHandles() uint64
	ShardCount() int
	ShardSkipped(index int) uint64
	ShardStats(index int) commonobs.SlotShardStats
}

// TelemetryProvider exposes operation-tracker counters as query-ready metrics.
type TelemetryProvider struct {
	source OperationTrackerTelemetrySource
}

// TelemetrySnapshot is a point-in-time copy of aggregate and shard-level
// operation-tracker pressure counters.
type TelemetrySnapshot struct {
	SkippedOperations          uint64
	DroppedRecords             uint64
	DroppedCompletedOperations uint64
	InvalidHandles             uint64
	Shards                     []TelemetryShardSnapshot
}

// TelemetryShardSnapshot is a point-in-time copy of one tracker shard's pressure.
type TelemetryShardSnapshot struct {
	SkippedOperations uint64
	Segments          int
	ActiveSlots       int
	FreeSlots         int
	CompletedSlots    int
}

// NewTelemetryProvider creates a metrics.telemetry snapshot provider.
func NewTelemetryProvider(source OperationTrackerTelemetrySource) *TelemetryProvider {
	return &TelemetryProvider{source: source}
}

// Snapshot copies source counters without mutating command-path state.
func (p *TelemetryProvider) Snapshot() TelemetrySnapshot {
	if p == nil || p.source == nil {
		return TelemetrySnapshot{}
	}

	shardCount := p.source.ShardCount()
	if shardCount < 1 {
		return TelemetrySnapshot{
			SkippedOperations:          p.source.SkippedOperations(),
			DroppedRecords:             p.source.DroppedRecords(),
			DroppedCompletedOperations: p.source.DroppedCompletedOperations(),
			InvalidHandles:             p.source.InvalidHandles(),
		}
	}

	shards := make([]TelemetryShardSnapshot, shardCount)
	for shardIndex := 0; shardIndex < shardCount; shardIndex++ {
		stats := p.source.ShardStats(shardIndex)
		shards[shardIndex] = TelemetryShardSnapshot{
			SkippedOperations: p.source.ShardSkipped(shardIndex),
			Segments:          stats.Segments,
			ActiveSlots:       stats.ActiveSlots,
			FreeSlots:         stats.FreeSlots,
			CompletedSlots:    stats.CompletedSlots,
		}
	}

	return TelemetrySnapshot{
		SkippedOperations:          p.source.SkippedOperations(),
		DroppedRecords:             p.source.DroppedRecords(),
		DroppedCompletedOperations: p.source.DroppedCompletedOperations(),
		InvalidHandles:             p.source.InvalidHandles(),
		Shards:                     shards,
	}
}

// QueryData returns Snapshot encoded as server-query key/value metrics.
func (p *TelemetryProvider) QueryData() map[string]string {
	return p.Snapshot().QueryData()
}

// QueryData encodes the snapshot as deterministic metrics.telemetry keys.
func (s TelemetrySnapshot) QueryData() map[string]string {
	metricsMap := make(map[string]string, 5+len(s.Shards)*5)
	metricsMap["telemetry.skipped_operations"] = strconv.FormatUint(s.SkippedOperations, 10)
	metricsMap["telemetry.dropped_records"] = strconv.FormatUint(s.DroppedRecords, 10)
	metricsMap["telemetry.dropped_completed"] = strconv.FormatUint(s.DroppedCompletedOperations, 10)
	metricsMap["telemetry.invalid_handles"] = strconv.FormatUint(s.InvalidHandles, 10)
	metricsMap["telemetry.shards.count"] = strconv.Itoa(len(s.Shards))

	for shardIndex, shardSnapshot := range s.Shards {
		prefix := fmt.Sprintf("telemetry.shard_%d.", shardIndex)
		metricsMap[prefix+"skipped"] = strconv.FormatUint(shardSnapshot.SkippedOperations, 10)
		metricsMap[prefix+"active"] = strconv.Itoa(shardSnapshot.ActiveSlots)
		metricsMap[prefix+"free"] = strconv.Itoa(shardSnapshot.FreeSlots)
		metricsMap[prefix+"completed"] = strconv.Itoa(shardSnapshot.CompletedSlots)
		metricsMap[prefix+"segments"] = strconv.Itoa(shardSnapshot.Segments)
	}

	return metricsMap
}
