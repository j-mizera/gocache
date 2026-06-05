package main

import (
	"errors"
	"testing"
	"time"

	apiEvents "gocache/api/events"
	apiobs "gocache/api/observability"
	ops "gocache/api/operations"
	commonobs "gocache/commons/observability"
	"gocache/pkg/cache"
	serverEvents "gocache/pkg/events"
	serverpkg "gocache/pkg/server"
)

func TestNewSteadyStateOperationTrackerManagerDefaults(t *testing.T) {
	manager := newSteadyStateOperationTrackerManager()
	if manager == nil {
		t.Fatal("newSteadyStateOperationTrackerManager returned nil")
	}
	if got := manager.ShardCount(); got != steadyStateOperationTrackerShardCount {
		t.Fatalf("ShardCount() = %d, want %d", got, steadyStateOperationTrackerShardCount)
	}

	wantFreeSlots := steadyStateOperationTrackerMinSegmentsPerShard * steadyStateOperationTrackerSegmentSize
	for shard := 0; shard < manager.ShardCount(); shard++ {
		stats := manager.ShardStats(shard)
		if stats.Segments != steadyStateOperationTrackerMinSegmentsPerShard {
			t.Fatalf("shard %d segments = %d, want %d", shard, stats.Segments, steadyStateOperationTrackerMinSegmentsPerShard)
		}
		if stats.FreeSlots != wantFreeSlots {
			t.Fatalf("shard %d free slots = %d, want %d", shard, stats.FreeSlots, wantFreeSlots)
		}
		if stats.ActiveSlots != 0 || stats.CompletedSlots != 0 {
			t.Fatalf("shard %d starts active/completed = %d/%d, want 0/0", shard, stats.ActiveSlots, stats.CompletedSlots)
		}
	}
}

func TestNewSteadyStateOperationTrackerManagerDrainsHolderLogRequest(t *testing.T) {
	manager := newSteadyStateOperationTrackerManager()
	operation := apiobs.InternalOperationIdentity(steadyStateOperationTrackerShardCount)
	handle, ok := manager.StartOperation(operation, apiobs.NewParentRef("client-1"), 0)
	if !ok {
		t.Fatal("StartOperation should allocate from production steady-state defaults")
	}

	scope := commonobs.NewOperationScope(manager, handle, operation, apiobs.NewOperationRef("cmd-1", "client-1"))
	if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("steady-state log")) {
		t.Fatal("scope Log should submit a log.request record")
	}
	if !scope.Finish(commonobs.SlotTerminalFinished) {
		t.Fatal("scope Finish should enqueue the completed operation")
	}

	var completed commonobs.CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation commonobs.CompletedOperation) {
		completed = operation
		completed.Records = append([]apiobs.TelemetryRecord(nil), operation.Records...)
	}); drained != 1 {
		t.Fatalf("drained %d completed operations, want 1", drained)
	}
	if completed.Status != commonobs.SlotTerminalFinished {
		t.Fatalf("completed status = %v, want finished", completed.Status)
	}
	if got := completed.Parent.String(); got != "client-1" {
		t.Fatalf("completed parent = %q, want client-1", got)
	}
	if len(completed.Records) != 1 {
		t.Fatalf("completed record count = %d, want 1", len(completed.Records))
	}
	record := completed.Records[0]
	if record.Kind != apiobs.TelemetryRecordLog {
		t.Fatalf("record kind = %v, want log.request", record.Kind)
	}
	if got := string(record.NameBytes()); got != "steady-state log" {
		t.Fatalf("record message = %q, want steady-state log", got)
	}
}

func TestConfigReloadTelemetryScopeRecordsReloadAndMemoryLimitLogs(t *testing.T) {
	manager := newSteadyStateOperationTrackerManager()
	reloadOp := ops.New(ops.TypeConfigReload, "")
	scope := startConfigReloadTelemetryScope(manager, reloadOp)
	if scope.IsZero() {
		t.Fatal("config reload telemetry scope should be active")
	}
	recordConfigReloadLog(scope, apiobs.TelemetryLogLevelInfo, "config reloaded", nil)

	cacheInstance := cache.New()
	cacheInstance.SetMemoryLimit(scope, 32, cache.EvictionLRU)
	reloadOp.Complete()
	if !finishLifecycleTelemetryScope(scope, reloadOp, commonobs.SlotTerminalFinished, "") {
		t.Fatal("finishLifecycleTelemetryScope should enqueue config reload operation")
	}

	completed := drainSingleCompletedOperation(t, manager)
	if completed.status != commonobs.SlotTerminalFinished {
		t.Fatalf("completed status = %v, want finished", completed.status)
	}
	logs := logRecords(completed.records)
	if len(logs) != 2 {
		t.Fatalf("log records = %d, want 2", len(logs))
	}
	assertLogRecord(t, logs[0], apiobs.TelemetryLogLevelInfo, "config reloaded")
	memoryRecord := logs[1]
	assertLogRecord(t, memoryRecord, apiobs.TelemetryLogLevelInfo, "memory limit updated")
	if memoryRecord.Number != 32*1024*1024 {
		t.Fatalf("memory limit record number = %d, want 32 MiB in bytes", memoryRecord.Number)
	}
	key, value, ok := memoryRecord.FieldBytes(0)
	if !ok || string(key) != "policy" || string(value) != "allkeys-lru" {
		t.Fatalf("memory limit field = %q/%q/%v, want policy/allkeys-lru/true", key, value, ok)
	}
}

func TestConfigReloadTelemetryScopeRecordsRestartWarning(t *testing.T) {
	manager := newSteadyStateOperationTrackerManager()
	reloadOp := ops.New(ops.TypeConfigReload, "")
	scope := startConfigReloadTelemetryScope(manager, reloadOp)
	if scope.IsZero() {
		t.Fatal("config reload telemetry scope should be active")
	}
	recordConfigReloadLog(scope, apiobs.TelemetryLogLevelWarn, "server address/port changes require a restart", nil)
	reloadOp.Complete()
	if !finishLifecycleTelemetryScope(scope, reloadOp, commonobs.SlotTerminalFinished, "") {
		t.Fatal("finishLifecycleTelemetryScope should enqueue config reload operation")
	}

	completed := drainSingleCompletedOperation(t, manager)
	if completed.status != commonobs.SlotTerminalFinished {
		t.Fatalf("completed status = %v, want finished", completed.status)
	}
	logs := logRecords(completed.records)
	if len(logs) != 1 {
		t.Fatalf("log records = %d, want 1", len(logs))
	}
	assertLogRecord(t, logs[0], apiobs.TelemetryLogLevelWarn, "server address/port changes require a restart")
}

func TestConfigReloadTelemetryScopeRecordsParseFailure(t *testing.T) {
	manager := newSteadyStateOperationTrackerManager()
	reloadOp := ops.New(ops.TypeConfigReload, "")
	scope := startConfigReloadTelemetryScope(manager, reloadOp)
	if scope.IsZero() {
		t.Fatal("config reload telemetry scope should be active")
	}
	parseErr := errors.New("bad config")
	recordConfigReloadLog(scope, apiobs.TelemetryLogLevelWarn, "failed to parse updated config", parseErr)
	reloadOp.Fail(parseErr.Error())
	if !finishLifecycleTelemetryScope(scope, reloadOp, commonobs.SlotTerminalFailed, parseErr.Error()) {
		t.Fatal("finishLifecycleTelemetryScope should enqueue failed config reload operation")
	}

	completed := drainSingleCompletedOperation(t, manager)
	if completed.status != commonobs.SlotTerminalFailed {
		t.Fatalf("completed status = %v, want failed", completed.status)
	}
	logs := logRecords(completed.records)
	if len(logs) != 1 {
		t.Fatalf("log records = %d, want 1", len(logs))
	}
	record := logs[0]
	assertLogRecord(t, record, apiobs.TelemetryLogLevelWarn, "failed to parse updated config")
	key, value, ok := record.FieldBytes(0)
	if !ok || string(key) != "error" || string(value) != parseErr.Error() {
		t.Fatalf("error field = %q/%q/%v, want error/%q/true", key, value, ok, parseErr.Error())
	}
}

func TestEmitShutdownSignalTelemetryDrainsBeforePluginShutdown(t *testing.T) {
	manager := newSteadyStateOperationTrackerManager()
	eventBus := serverEvents.NewBus()
	received := make(chan apiEvents.Event, 4)
	eventBus.Subscribe("shutdown-test", []apiEvents.Type{apiEvents.ServerShutdown}, func(event apiEvents.Event) {
		received <- event
	})
	worker := serverpkg.NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(eventBus)

	emitShutdownSignalTelemetry(manager, worker, "signal", "parent-shutdown")

	select {
	case event := <-received:
		if event.Proto.Type != string(apiEvents.ServerShutdown) {
			t.Fatalf("event type = %q, want %q", event.Proto.Type, apiEvents.ServerShutdown)
		}
		payload := event.Proto.GetServerShutdown()
		if payload == nil || payload.Reason != "signal" {
			t.Fatalf("shutdown payload = %#v, want reason signal", payload)
		}
		if event.Proto.OperationId == "" {
			t.Fatal("server.shutdown should carry operation_id")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for server.shutdown event")
	}
}

type drainedMainOperation struct {
	status  commonobs.SlotTerminalStatus
	records []apiobs.TelemetryRecord
}

func drainSingleCompletedOperation(t *testing.T, manager *commonobs.SlotOperationTrackerManager) drainedMainOperation {
	t.Helper()
	var completed drainedMainOperation
	drained := 0
	for shard := 0; shard < manager.ShardCount(); shard++ {
		drained += manager.DrainCompletedShard(shard, func(operation commonobs.CompletedOperation) {
			completed.status = operation.Status
			completed.records = append(completed.records[:0], operation.Records...)
		})
	}
	if drained != 1 {
		t.Fatalf("drained %d completed operations, want 1", drained)
	}
	return completed
}

func logRecords(records []apiobs.TelemetryRecord) []apiobs.TelemetryRecord {
	logs := make([]apiobs.TelemetryRecord, 0, len(records))
	for _, record := range records {
		if record.Kind == apiobs.TelemetryRecordLog {
			logs = append(logs, record)
		}
	}
	return logs
}

func assertLogRecord(t *testing.T, record apiobs.TelemetryRecord, level apiobs.TelemetryLogLevel, message string) {
	t.Helper()
	if record.Kind != apiobs.TelemetryRecordLog || record.Level != level {
		t.Fatalf("record kind/level = %v/%v, want log/%v", record.Kind, record.Level, level)
	}
	if got := string(record.NameBytes()); got != message {
		t.Fatalf("record message = %q, want %q", got, message)
	}
}

func TestNewSteadyStateOperationTrackerManagerRetainsInitialSameShardBurst(t *testing.T) {
	manager := newSteadyStateOperationTrackerManager()
	operations := steadyStateOperationTrackerMinSegmentsPerShard * steadyStateOperationTrackerSegmentSize

	for i := 1; i <= operations; i++ {
		operation := apiobs.InternalOperationIdentity(i * steadyStateOperationTrackerShardCount)
		handle, ok := manager.StartOperation(operation, apiobs.NewParentRef("client-1"), 0)
		if !ok {
			t.Fatalf("StartOperation(%d) returned no slot before initial shard capacity was exhausted", i)
		}
		scope := commonobs.NewOperationScope(manager, handle, operation, apiobs.NewOperationRef("", "client-1"))
		if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("burst log")) {
			t.Fatalf("Log(%d) should submit before initial shard capacity was exhausted", i)
		}
		if !scope.Finish(commonobs.SlotTerminalFinished) {
			t.Fatalf("Finish(%d) should enqueue without completed-ring drop", i)
		}
	}

	if dropped := manager.DroppedCompletedOperations(); dropped != 0 {
		t.Fatalf("DroppedCompletedOperations() = %d, want 0 for initially accepted same-shard burst", dropped)
	}
	stats := manager.ShardStats(0)
	if stats.CompletedSlots != operations {
		t.Fatalf("completed slots = %d, want %d", stats.CompletedSlots, operations)
	}
	if stats.FreeSlots != 0 {
		t.Fatalf("free slots = %d, want 0 after retaining initial completed burst", stats.FreeSlots)
	}

	nextOperation := apiobs.InternalOperationIdentity((operations + 1) * steadyStateOperationTrackerShardCount)
	if _, ok := manager.StartOperation(nextOperation, apiobs.NewParentRef("client-1"), 0); ok {
		t.Fatal("StartOperation should skip rather than recycle/drop retained completed operations without a drain worker")
	}
	if skipped := manager.SkippedOperations(); skipped != 1 {
		t.Fatalf("SkippedOperations() = %d, want 1 after initial capacity is retained", skipped)
	}

	drained := manager.DrainCompletedShard(0, nil)
	if drained != operations {
		t.Fatalf("drained %d retained operations, want %d", drained, operations)
	}
}
