package server

// ADR-0037 PATH B benchmark harness for tmpfs TelemetryOperation serialization.
// Refresh results with:
//
// 	go test -bench=. -benchmem -count=1 ./pkg/server/ -run=^B
// 	go test -race -count=1 ./pkg/server/ -run=TestTelemetrySerializeAllocs
//
// The old event-emitter path must materialize at least one context map and copy
// one key/value pair per context entry before sink fanout. These benchmarks keep
// that context-copy comparison next to the protobuf serialization measurement.
//
// Standard protobuf local result on 2026-06-25, linux/amd64, AMD Ryzen 9 7900X:
//
// 	BenchmarkTelemetryOperationSerialize-24                                253491  4331 ns/op  5168 B/op  42 allocs/op
// 	BenchmarkTelemetryOperationOldVsNewPath/old_copy_operation_context-24 3535372   337.3 ns/op 336 B/op   2 allocs/op
// 	BenchmarkTelemetryOperationOldVsNewPath/new_telemetry_operation_serialize-24 282706 4285 ns/op 5168 B/op 42 allocs/op
//
// TestTelemetrySerializeAllocsPerRun guards the vtprotobuf serialization path.

import (
	"strconv"
	"testing"

	gcpcv1 "gocache/api/gcpc/v1"
	apiobs "gocache/api/observability"
	commonobs "gocache/commons/observability"
)

var serializedTelemetryOperationSink []byte
var copiedTelemetryOperationContextSink map[string]string

type telemetrySerializationBenchmarkFixture struct {
	manager   *commonobs.SlotOperationTrackerManager
	operation commonobs.CompletedOperation
}

func BenchmarkTelemetryOperationSerialize(b *testing.B) {
	fixture := newTelemetrySerializationBenchmarkFixture()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		serializedOperation, err := marshalCompletedOperationTelemetryForBenchmark(fixture.manager, fixture.operation)
		if err != nil {
			b.Fatalf("marshal completed operation telemetry: %v", err)
		}
		serializedTelemetryOperationSink = serializedOperation
	}
}

func BenchmarkTelemetryOperationOldVsNewPath(b *testing.B) {
	fixture := newTelemetrySerializationBenchmarkFixture()
	worker := &OperationTrackerDrainWorker{manager: fixture.manager}

	b.Run("old_copy_operation_context", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			copiedTelemetryOperationContextSink = worker.copyOperationContext(fixture.operation)
		}
	})

	b.Run("new_telemetry_operation_serialize", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			serializedOperation, err := marshalCompletedOperationTelemetryForBenchmark(fixture.manager, fixture.operation)
			if err != nil {
				b.Fatalf("marshal completed operation telemetry: %v", err)
			}
			serializedTelemetryOperationSink = serializedOperation
		}
	})
}

func TestTelemetrySerializeAllocsPerRun(t *testing.T) {
	fixture := newTelemetrySerializationBenchmarkFixture()

	allocs := testing.AllocsPerRun(100, func() {
		serializedOperation, err := marshalCompletedOperationTelemetryForBenchmark(fixture.manager, fixture.operation)
		if err != nil {
			t.Fatalf("marshal completed operation telemetry: %v", err)
		}
		serializedTelemetryOperationSink = serializedOperation
	})

	// Current baseline: 41 allocs/op with vtprotobuf MarshalVT.
	// Standard protobuf was 42 allocs/op — MarshalVT gives modest improvement.
	// Dramatic reduction (<5 allocs) requires MarshalToVT + sync.Pool'd buffers (future optimization).
	// Threshold allows headroom for protobuf version changes.
	const maxAllocs = 45
	if allocs > maxAllocs {
		t.Fatalf("allocs per operation = %v, want <= %d (baseline 41 with vtprotobuf)", allocs, maxAllocs)
	}
	t.Logf("allocs per operation: %v (baseline 41, threshold %d)", allocs, maxAllocs)
}

func newTelemetrySerializationBenchmarkFixture() telemetrySerializationBenchmarkFixture {
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   10,
		CompletedRingPerShard: 1,
	})
	contextVersion := manager.UpdateConnectionContextStrings(
		apiobs.ConnectionIdentity(7),
		"tenant", "acme",
		"role", "writer",
		"request", "req-123",
		"db", "0",
		"client", "bench-client",
	)

	const operationID = apiobs.InternalOperationIdentity(42)
	operation := commonobs.CompletedOperation{
		Operation:      operationID,
		Parent:         apiobs.NewParentRef("parent-operation"),
		ContextVersion: contextVersion,
		Status:         commonobs.SlotTerminalFinished,
		Records: []apiobs.TelemetryRecord{
			benchmarkTelemetryRecord(apiobs.TelemetryRecordOperationStart, operationID, "operation:start", "type", "command"),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordCommandStart, operationID, "GET", "key", "cache:user:1"),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordContextUpdate, operationID, "context:update", "role", "reader"),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordLog, operationID, "cache lookup started", "component", "pipeline"),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordEvent, operationID, "cache.lookup", "hit", "false"),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordCommandFinish, operationID, "GET", "status", "ok"),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordCommandStart, operationID, "SET", "key", "cache:user:1"),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordContextRemove, operationID, "context:remove", "role", ""),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordDrop, operationID, "drop", "reason", "overflow"),
			benchmarkTelemetryRecord(apiobs.TelemetryRecordOperationFinish, operationID, "operation:finish", "status", "finished"),
		},
	}

	return telemetrySerializationBenchmarkFixture{manager: manager, operation: operation}
}

func marshalCompletedOperationTelemetryForBenchmark(manager *commonobs.SlotOperationTrackerManager, operation commonobs.CompletedOperation) ([]byte, error) {
	telemetryOperation := &gcpcv1.TelemetryOperation{
		OperationId:    strconv.FormatInt(int64(operation.Operation), 10),
		TelemetryItems: make([]*gcpcv1.TelemetryItem, 0, len(operation.Records)),
	}
	if manager != nil && !operation.ContextVersion.IsZero() {
		manager.VisitConnectionContextVersion(operation.ContextVersion, func(contextKey, contextValue string) bool {
			telemetryOperation.InitialContext = append(telemetryOperation.InitialContext, &gcpcv1.Tag{
				Key:   []byte(contextKey),
				Value: []byte(contextValue),
			})
			return true
		})
	}
	for recordIndex := range operation.Records {
		record := operation.Records[recordIndex]
		telemetryKind := gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_UNSPECIFIED
		switch record.Kind {
		case apiobs.TelemetryRecordOperationStart:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_START
		case apiobs.TelemetryRecordOperationFinish:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH
		case apiobs.TelemetryRecordCommandStart:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_START
		case apiobs.TelemetryRecordCommandFinish:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_FINISH
		case apiobs.TelemetryRecordContextUpdate:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_UPDATE
		case apiobs.TelemetryRecordContextRemove:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_REMOVE
		case apiobs.TelemetryRecordLog:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_LOG
		case apiobs.TelemetryRecordEvent:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_EVENT
		case apiobs.TelemetryRecordDrop:
			telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_DROP
		}
		telemetryOperation.TelemetryItems = append(telemetryOperation.TelemetryItems, &gcpcv1.TelemetryItem{
			Kind:    telemetryKind,
			Payload: record.PayloadBytes(),
		})
	}
	return telemetryOperation.MarshalVT()
}

func benchmarkTelemetryRecord(kind apiobs.TelemetryRecordKind, operation apiobs.InternalOperationIdentity, name string, fieldKey string, fieldValue string) apiobs.TelemetryRecord {
	record := apiobs.NewTelemetryRecord(kind, operation)
	record.SetNameString(name)
	if fieldValue == "" {
		record.SetPayload([]byte{1, byte(len(fieldKey))})
		copy(record.Payload[2:], fieldKey)
		record.PayloadLen = uint16(2 + len(fieldKey))
		return record
	}
	if !record.AddFieldString(fieldKey, fieldValue) {
		return record
	}
	return record
}
