package handler

import (
	"errors"
	"testing"

	apiobs "gocache/api/observability"
	commonobs "gocache/commons/observability"
)

func TestSubmitHandlerErrorLogRecordsListWriteBackFailureThroughHolder(t *testing.T) {
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(201, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate slot")
	}
	scope := commonobs.NewOperationScope(manager, handle, 201, apiobs.NewOperationRef("list-writeback-test", ""))

	if !submitHandlerErrorLog(scope, "unexpected error on pop write-back", "list-key", errors.New("write failed")) {
		t.Fatal("handler log should be accepted")
	}
	if !scope.Finish(commonobs.SlotTerminalFailed) {
		t.Fatal("scope should finish")
	}

	var record apiobs.TelemetryRecord
	if drained := manager.DrainCompletedShard(0, func(operation commonobs.CompletedOperation) {
		if len(operation.Records) != 1 {
			t.Fatalf("record count = %d, want 1", len(operation.Records))
		}
		record = operation.Records[0]
	}); drained != 1 {
		t.Fatalf("drained %d operations, want 1", drained)
	}
	if record.Kind != apiobs.TelemetryRecordLog || record.Level != apiobs.TelemetryLogLevelError {
		t.Fatalf("record kind/level = %v/%v, want log/error", record.Kind, record.Level)
	}
	if string(record.NameBytes()) != "unexpected error on pop write-back" {
		t.Fatalf("log message = %q, want unexpected error on pop write-back", record.NameBytes())
	}
	key, value, ok := record.FieldBytes(0)
	if !ok || string(key) != "key" || string(value) != "list-key" {
		t.Fatalf("field[0] = %q/%q/%v, want key/list-key/true", key, value, ok)
	}
	key, value, ok = record.FieldBytes(1)
	if !ok || string(key) != "error" || string(value) != "write failed" {
		t.Fatalf("field[1] = %q/%q/%v, want error/write failed/true", key, value, ok)
	}
}
