package observability

import (
	"testing"

	apiobs "gocache/api/observability"
)

func TestOperationScopeRecordsLogAndFinishes(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   2,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(11, apiobs.NewParentRef("parent"), 0)
	if !ok {
		t.Fatal("StartOperation should allocate slot")
	}
	scope := NewOperationScope(manager, handle, 11, apiobs.NewOperationRef("public-op", "parent"))
	if scope.IsZero() {
		t.Fatal("scope should be usable")
	}
	if scope.Operation() != 11 {
		t.Fatalf("Operation() = %d, want 11", scope.Operation())
	}
	if scope.Ref().ID != "public-op" || scope.Ref().ParentID != "parent" {
		t.Fatalf("Ref() = %+v, want public-op/parent", scope.Ref())
	}
	if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("startup ready")) {
		t.Fatal("Log should be accepted")
	}
	if !scope.Finish(SlotTerminalFinished) {
		t.Fatal("Finish should enqueue completed operation")
	}

	var completed CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		completed = cloneCompletedOperation(operation)
	}); drained != 1 {
		t.Fatalf("drained %d operations, want 1", drained)
	}
	if completed.Operation != 11 {
		t.Fatalf("completed operation = %d, want 11", completed.Operation)
	}
	if len(completed.Records) != 1 {
		t.Fatalf("record count = %d, want 1", len(completed.Records))
	}
	record := completed.Records[0]
	if record.Kind != apiobs.TelemetryRecordLog || record.Level != apiobs.TelemetryLogLevelInfo {
		t.Fatalf("record kind/level = %v/%v, want log/info", record.Kind, record.Level)
	}
	if string(record.NameBytes()) != "startup ready" {
		t.Fatalf("log message = %q, want startup ready", record.NameBytes())
	}
	if record.TimestampUnixNano == 0 {
		t.Fatal("scope log timestamp should be set")
	}
}

func TestOperationScopeRecordsContextMutations(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   2,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(12, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate slot")
	}
	scope := NewOperationScope(manager, handle, 12, apiobs.NewOperationRef("public-op", ""))
	if !scope.ContextUpdate([]byte("tenant"), []byte("acme"), []byte("role"), []byte("reader")) {
		t.Fatal("ContextUpdate should be accepted")
	}
	if !scope.ContextRemove([]byte("role")) {
		t.Fatal("ContextRemove should be accepted")
	}
	if !scope.Finish(SlotTerminalFinished) {
		t.Fatal("Finish should enqueue completed operation")
	}

	var completed CompletedOperation
	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		completed = cloneCompletedOperation(operation)
	})
	if len(completed.Records) != 2 {
		t.Fatalf("record count = %d, want 2", len(completed.Records))
	}
	update := completed.Records[0]
	if update.Kind != apiobs.TelemetryRecordContextUpdate || update.Operation != 12 {
		t.Fatalf("update record = %+v, want context.update operation 12", update)
	}
	if update.Payload[0] != 2 {
		t.Fatalf("update pair count = %d, want 2", update.Payload[0])
	}
	remove := completed.Records[1]
	if remove.Kind != apiobs.TelemetryRecordContextRemove || remove.Operation != 12 {
		t.Fatalf("remove record = %+v, want context.remove operation 12", remove)
	}
	if remove.Payload[0] != 1 {
		t.Fatalf("remove key count = %d, want 1", remove.Payload[0])
	}
}

func TestOperationScopeForcesRecordOperation(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(22, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate slot")
	}
	scope := NewOperationScope(manager, handle, 22, apiobs.NewOperationRef("public-op", ""))
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordEvent, 999)
	record.SetName([]byte("server.ready"))
	if !scope.Record(record) {
		t.Fatal("Record should be accepted")
	}
	if !scope.Finish(SlotTerminalFinished) {
		t.Fatal("Finish should enqueue completed operation")
	}

	var completed CompletedOperation
	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		completed = cloneCompletedOperation(operation)
	})
	if len(completed.Records) != 1 {
		t.Fatalf("record count = %d, want 1", len(completed.Records))
	}
	if completed.Records[0].Operation != 22 {
		t.Fatalf("record operation = %d, want scope operation 22", completed.Records[0].Operation)
	}
}

func TestOperationScopeZeroRejectsSubmissions(t *testing.T) {
	var scope OperationScope
	if !scope.IsZero() {
		t.Fatal("zero scope should report IsZero")
	}
	if scope.Log(apiobs.TelemetryLogLevelInfo, []byte("ignored")) {
		t.Fatal("zero scope Log should be rejected")
	}
	if scope.Record(apiobs.NewTelemetryRecord(apiobs.TelemetryRecordEvent, 1)) {
		t.Fatal("zero scope Record should be rejected")
	}
	if scope.ContextUpdate([]byte("tenant"), []byte("acme")) {
		t.Fatal("zero scope ContextUpdate should be rejected")
	}
	if scope.ContextRemove([]byte("tenant")) {
		t.Fatal("zero scope ContextRemove should be rejected")
	}
	if scope.Finish(SlotTerminalFinished) {
		t.Fatal("zero scope Finish should be rejected")
	}
}
