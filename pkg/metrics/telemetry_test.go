package metrics

import (
	"reflect"
	"testing"

	commonobs "gocache/commons/observability"
)

type telemetrySourceStub struct {
	skippedOperations          uint64
	droppedRecords             uint64
	droppedCompletedOperations uint64
	invalidHandles             uint64
	shardSkippedOperations     []uint64
	shardStats                 []commonobs.SlotShardStats
}

func (s telemetrySourceStub) SkippedOperations() uint64 {
	return s.skippedOperations
}

func (s telemetrySourceStub) DroppedRecords() uint64 {
	return s.droppedRecords
}

func (s telemetrySourceStub) DroppedCompletedOperations() uint64 {
	return s.droppedCompletedOperations
}

func (s telemetrySourceStub) InvalidHandles() uint64 {
	return s.invalidHandles
}

func (s telemetrySourceStub) ShardCount() int {
	return len(s.shardStats)
}

func (s telemetrySourceStub) ShardSkipped(index int) uint64 {
	return s.shardSkippedOperations[index]
}

func (s telemetrySourceStub) ShardStats(index int) commonobs.SlotShardStats {
	return s.shardStats[index]
}

func TestTelemetryProviderNilSourceReturnsZeroQueryData(t *testing.T) {
	tests := []struct {
		name     string
		provider *TelemetryProvider
	}{
		{name: "nil provider", provider: nil},
		{name: "nil source", provider: NewTelemetryProvider(nil)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			metricsMap := tc.provider.QueryData()
			expectedMetrics := map[string]string{
				"telemetry.skipped_operations": "0",
				"telemetry.dropped_records":    "0",
				"telemetry.dropped_completed":  "0",
				"telemetry.invalid_handles":    "0",
				"telemetry.shards.count":       "0",
			}

			if !reflect.DeepEqual(metricsMap, expectedMetrics) {
				t.Fatalf("QueryData() = %#v, want %#v", metricsMap, expectedMetrics)
			}
		})
	}
}

func TestTelemetryProviderSnapshotsAggregateAndShardPressure(t *testing.T) {
	source := telemetrySourceStub{
		skippedOperations:          11,
		droppedRecords:             22,
		droppedCompletedOperations: 33,
		invalidHandles:             44,
		shardSkippedOperations:     []uint64{55, 66},
		shardStats: []commonobs.SlotShardStats{
			{Segments: 2, ActiveSlots: 3, FreeSlots: 4, CompletedSlots: 5},
			{Segments: 6, ActiveSlots: 7, FreeSlots: 8, CompletedSlots: 9},
		},
	}
	provider := NewTelemetryProvider(source)

	snapshot := provider.Snapshot()
	expectedSnapshot := TelemetrySnapshot{
		SkippedOperations:          11,
		DroppedRecords:             22,
		DroppedCompletedOperations: 33,
		InvalidHandles:             44,
		Shards: []TelemetryShardSnapshot{
			{SkippedOperations: 55, Segments: 2, ActiveSlots: 3, FreeSlots: 4, CompletedSlots: 5},
			{SkippedOperations: 66, Segments: 6, ActiveSlots: 7, FreeSlots: 8, CompletedSlots: 9},
		},
	}

	if !reflect.DeepEqual(snapshot, expectedSnapshot) {
		t.Fatalf("Snapshot() = %#v, want %#v", snapshot, expectedSnapshot)
	}
}

func TestTelemetryProviderQueryDataUsesDeterministicShardKeys(t *testing.T) {
	source := telemetrySourceStub{
		skippedOperations:          1,
		droppedRecords:             2,
		droppedCompletedOperations: 3,
		invalidHandles:             4,
		shardSkippedOperations:     []uint64{5, 10},
		shardStats: []commonobs.SlotShardStats{
			{Segments: 6, ActiveSlots: 7, FreeSlots: 8, CompletedSlots: 9},
			{Segments: 11, ActiveSlots: 12, FreeSlots: 13, CompletedSlots: 14},
		},
	}

	metricsMap := NewTelemetryProvider(source).QueryData()
	expectedMetrics := map[string]string{
		"telemetry.skipped_operations": "1",
		"telemetry.dropped_records":    "2",
		"telemetry.dropped_completed":  "3",
		"telemetry.invalid_handles":    "4",
		"telemetry.shards.count":       "2",
		"telemetry.shard_0.skipped":    "5",
		"telemetry.shard_0.active":     "7",
		"telemetry.shard_0.free":       "8",
		"telemetry.shard_0.completed":  "9",
		"telemetry.shard_0.segments":   "6",
		"telemetry.shard_1.skipped":    "10",
		"telemetry.shard_1.active":     "12",
		"telemetry.shard_1.free":       "13",
		"telemetry.shard_1.completed":  "14",
		"telemetry.shard_1.segments":   "11",
	}

	if !reflect.DeepEqual(metricsMap, expectedMetrics) {
		t.Fatalf("QueryData() = %#v, want %#v", metricsMap, expectedMetrics)
	}
}

func TestTelemetryTopicDoesNotReplaceCommandMetricsTopic(t *testing.T) {
	if TelemetryTopic != "metrics.telemetry" {
		t.Fatalf("TelemetryTopic = %q, want metrics.telemetry", TelemetryTopic)
	}
	if CommandsTopic != "metrics.commands" {
		t.Fatalf("CommandsTopic = %q, want metrics.commands", CommandsTopic)
	}
	if TelemetryTopic == CommandsTopic {
		t.Fatalf("TelemetryTopic and CommandsTopic must stay distinct")
	}
}
