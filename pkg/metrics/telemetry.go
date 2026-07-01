package metrics

import (
	"fmt"
	"strconv"
	"strings"

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
	CommandsTotal() uint64
	BatchesTotal() uint64
	OperationsStarted() uint64
	OperationsCompleted() uint64
	ShardCount() int
	ShardSkipped(index int) uint64
	ShardStats(index int) commonobs.SlotShardStats
}

// TelemetrySubscriberSource provides per-subscriber tmpfs delivery health.
// Implemented by the plugin manager to expose per-plugin telemetry writer stats.
type TelemetrySubscriberSource interface {
	SubscriberStats() []TelemetrySubscriberSnapshot
}

// TelemetryProvider exposes operation-tracker counters as query-ready metrics.
type TelemetryProvider struct {
	source           OperationTrackerTelemetrySource
	subscriberSource TelemetrySubscriberSource
}

// TelemetrySnapshot is a point-in-time copy of aggregate and shard-level
// operation-tracker pressure counters.
type TelemetrySnapshot struct {
	SkippedOperations          uint64
	DroppedRecords             uint64
	DroppedCompletedOperations uint64
	InvalidHandles             uint64
	CommandsTotal              uint64
	BatchesTotal               uint64
	OperationsStarted          uint64
	OperationsCompleted        uint64
	Shards                     []TelemetryShardSnapshot
	Subscribers                []TelemetrySubscriberSnapshot
}

// TelemetryShardSnapshot is a point-in-time copy of one tracker shard's pressure.
type TelemetryShardSnapshot struct {
	SkippedOperations uint64
	Segments          int
	ActiveSlots       int
	FreeSlots         int
	CompletedSlots    int
}

// TelemetrySubscriberSnapshot is a point-in-time copy of one telemetry
// subscriber's tmpfs delivery health.
type TelemetrySubscriberSnapshot struct {
	Name            string
	RecordsWritten  uint64
	BytesWritten    uint64
	WriteErrors     uint64
	OverflowDropped uint64
	WriteOffset     uint64
	ConsumedOffset  uint64
}

// NewTelemetryProvider creates a metrics.telemetry snapshot provider.
func NewTelemetryProvider(source OperationTrackerTelemetrySource) *TelemetryProvider {
	return &TelemetryProvider{source: source}
}

// SetSubscriberSource wires per-subscriber tmpfs delivery health into
// telemetry snapshots. Pass nil to clear.
func (p *TelemetryProvider) SetSubscriberSource(source TelemetrySubscriberSource) {
	if p == nil {
		return
	}
	p.subscriberSource = source
}

// Snapshot copies source counters without mutating command-path state.
func (p *TelemetryProvider) Snapshot() TelemetrySnapshot {
	if p == nil || p.source == nil {
		return TelemetrySnapshot{}
	}

	var subscribers []TelemetrySubscriberSnapshot
	if p.subscriberSource != nil {
		subscribers = p.subscriberSource.SubscriberStats()
	}
	shardCount := p.source.ShardCount()
	if shardCount < 1 {
		return TelemetrySnapshot{
			SkippedOperations:          p.source.SkippedOperations(),
			DroppedRecords:             p.source.DroppedRecords(),
			DroppedCompletedOperations: p.source.DroppedCompletedOperations(),
			InvalidHandles:             p.source.InvalidHandles(),
			CommandsTotal:              p.source.CommandsTotal(),
			BatchesTotal:               p.source.BatchesTotal(),
			OperationsStarted:          p.source.OperationsStarted(),
			OperationsCompleted:        p.source.OperationsCompleted(),
			Subscribers:                subscribers,
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
		CommandsTotal:              p.source.CommandsTotal(),
		BatchesTotal:               p.source.BatchesTotal(),
		OperationsStarted:          p.source.OperationsStarted(),
		OperationsCompleted:        p.source.OperationsCompleted(),
		Shards:                     shards,
		Subscribers:                subscribers,
	}
}

// QueryData returns Snapshot encoded as server-query key/value metrics.
func (p *TelemetryProvider) QueryData() map[string]string {
	return p.Snapshot().QueryData()
}

// QueryData encodes the snapshot as deterministic metrics.telemetry keys.
func (s TelemetrySnapshot) QueryData() map[string]string {
	metricsMap := make(map[string]string, 9+len(s.Shards)*5+len(s.Subscribers)*7+1)
	metricsMap["telemetry.skipped_operations"] = strconv.FormatUint(s.SkippedOperations, 10)
	metricsMap["telemetry.dropped_records"] = strconv.FormatUint(s.DroppedRecords, 10)
	metricsMap["telemetry.dropped_completed"] = strconv.FormatUint(s.DroppedCompletedOperations, 10)
	metricsMap["telemetry.invalid_handles"] = strconv.FormatUint(s.InvalidHandles, 10)
	metricsMap["telemetry.commands_total"] = strconv.FormatUint(s.CommandsTotal, 10)
	metricsMap["telemetry.batches_total"] = strconv.FormatUint(s.BatchesTotal, 10)
	metricsMap["telemetry.operations_started"] = strconv.FormatUint(s.OperationsStarted, 10)
	metricsMap["telemetry.operations_completed"] = strconv.FormatUint(s.OperationsCompleted, 10)
	metricsMap["telemetry.shards.count"] = strconv.Itoa(len(s.Shards))

	for shardIndex, shardSnapshot := range s.Shards {
		prefix := fmt.Sprintf("telemetry.shard_%d.", shardIndex)
		metricsMap[prefix+"skipped"] = strconv.FormatUint(shardSnapshot.SkippedOperations, 10)
		metricsMap[prefix+"active"] = strconv.Itoa(shardSnapshot.ActiveSlots)
		metricsMap[prefix+"free"] = strconv.Itoa(shardSnapshot.FreeSlots)
		metricsMap[prefix+"completed"] = strconv.Itoa(shardSnapshot.CompletedSlots)
		metricsMap[prefix+"segments"] = strconv.Itoa(shardSnapshot.Segments)
	}

	if len(s.Subscribers) > 0 {
		names := make([]string, len(s.Subscribers))
		for subscriberIndex, subscriberSnapshot := range s.Subscribers {
			prefix := fmt.Sprintf("telemetry.subscriber_%d.", subscriberIndex)
			metricsMap[prefix+"name"] = subscriberSnapshot.Name
			metricsMap[prefix+"records_written_total"] = strconv.FormatUint(subscriberSnapshot.RecordsWritten, 10)
			metricsMap[prefix+"bytes_written_total"] = strconv.FormatUint(subscriberSnapshot.BytesWritten, 10)
			metricsMap[prefix+"write_errors_total"] = strconv.FormatUint(subscriberSnapshot.WriteErrors, 10)
			metricsMap[prefix+"overflow_dropped"] = strconv.FormatUint(subscriberSnapshot.OverflowDropped, 10)
			metricsMap[prefix+"write_offset"] = strconv.FormatUint(subscriberSnapshot.WriteOffset, 10)
			metricsMap[prefix+"consumed_offset"] = strconv.FormatUint(subscriberSnapshot.ConsumedOffset, 10)
			names[subscriberIndex] = subscriberSnapshot.Name
		}
		metricsMap["telemetry.subscribers"] = strings.Join(names, ",")
	}

	return metricsMap
}
