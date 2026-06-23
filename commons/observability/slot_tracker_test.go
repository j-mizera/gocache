package observability

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	apiobs "gocache/api/observability"
)

type contextReleaseCounter struct {
	counts map[apiobs.ConnectionContextVersion]int
}

func (c *contextReleaseCounter) release(version apiobs.ConnectionContextVersion) bool {
	if version == 0 {
		return true
	}
	if c.counts == nil {
		c.counts = make(map[apiobs.ConnectionContextVersion]int)
	}
	c.counts[version]++
	return true
}

func (c *contextReleaseCounter) count(version apiobs.ConnectionContextVersion) int {
	if c.counts == nil {
		return 0
	}
	return c.counts[version]
}

func cloneCompletedOperation(operation CompletedOperation) CompletedOperation {
	operation.Records = append([]apiobs.TelemetryRecord(nil), operation.Records...)
	if operation.ContextOverlay != nil {
		clone := make(map[string]string, len(operation.ContextOverlay))
		for key, value := range operation.ContextOverlay {
			clone[key] = value
		}
		operation.ContextOverlay = clone
	}
	return operation
}

func trackerSnapshot(manager *SlotOperationTrackerManager) map[string]string {
	snapshot := make(map[string]string, 3+manager.ShardCount()*4)
	snapshot["operation_tracker.skipped_operations"] = strconv.FormatUint(manager.SkippedOperations(), 10)
	snapshot["operation_tracker.dropped_records"] = strconv.FormatUint(manager.DroppedRecords(), 10)
	snapshot["operation_tracker.dropped_completed"] = strconv.FormatUint(manager.DroppedCompletedOperations(), 10)
	for shardIndex := 0; shardIndex < manager.ShardCount(); shardIndex++ {
		prefix := fmt.Sprintf("operation_tracker.shard_%d.", shardIndex)
		snapshot[prefix+"skipped"] = strconv.FormatUint(manager.ShardSkipped(shardIndex), 10)
		snapshot[prefix+"active"] = strconv.Itoa(manager.ShardActiveSlots(shardIndex))
		snapshot[prefix+"free"] = strconv.Itoa(manager.ShardFreeSlots(shardIndex))
		snapshot[prefix+"completed"] = strconv.Itoa(manager.ShardCompletedSlots(shardIndex))
	}
	return snapshot
}

func TestSlotTrackerLifecycleDrainsCompletedOperation(t *testing.T) {
	releases := &contextReleaseCounter{}
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:              1,
		MinSegmentsPerShard:     1,
		MaxSegmentsPerShard:     1,
		SegmentSize:             2,
		RecordsPerOperation:     4,
		CompletedRingPerShard:   2,
		ReleaseContextVersionFn: releases.release,
	})

	parent := apiobs.NewParentRef("parent-op")
	version := apiobs.ConnectionContextVersion(7)
	handle, ok := manager.StartOperation(11, parent, version)
	if !ok {
		t.Fatal("StartOperation should allocate a preallocated slot")
	}
	startCommand := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 11)
	startCommand.SetName([]byte("GET"))
	finishCommand := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandFinish, 11)
	finishCommand.Number = 0
	if !manager.RecordTelemetry(handle, startCommand) {
		t.Fatal("RecordTelemetry command start should fit")
	}
	if !manager.RecordTelemetry(handle, finishCommand) {
		t.Fatal("RecordTelemetry command finish should fit")
	}
	if !manager.FinishOperation(handle, SlotTerminalFinished) {
		t.Fatal("FinishOperation should enqueue completed operation")
	}

	var completed []CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		completed = append(completed, cloneCompletedOperation(operation))
	}); drained != 1 {
		t.Fatalf("DrainCompletedShard drained %d operations, want 1", drained)
	}
	if len(completed) != 1 {
		t.Fatalf("completed count = %d, want 1", len(completed))
	}
	operation := completed[0]
	if operation.Operation != 11 {
		t.Fatalf("operation id = %d, want 11", operation.Operation)
	}
	if operation.ContextVersion != version {
		t.Fatalf("context version = %d, want %d", operation.ContextVersion, version)
	}
	if operation.Parent.String() != "parent-op" {
		t.Fatalf("parent = %q, want parent-op", operation.Parent.String())
	}
	if operation.Status != SlotTerminalFinished {
		t.Fatalf("status = %v, want finished", operation.Status)
	}
	if len(operation.Records) != 2 {
		t.Fatalf("record count = %d, want 2", len(operation.Records))
	}
	if operation.Records[0].Kind != apiobs.TelemetryRecordCommandStart || operation.Records[1].Kind != apiobs.TelemetryRecordCommandFinish {
		t.Fatalf("record kinds = %v, %v; want command start/finish", operation.Records[0].Kind, operation.Records[1].Kind)
	}
	if releases.count(version) != 1 {
		t.Fatalf("context releases for version %d = %d, want 1", version, releases.count(version))
	}
	if manager.RecordTelemetry(handle, startCommand) {
		t.Fatal("stale completed handle should not accept telemetry after drain/reset")
	}
	if manager.InvalidHandles() == 0 {
		t.Fatal("stale completed handle should increment invalid handle counter")
	}
}

func TestSlotTrackerNoSlotSkipsWithoutContextRelease(t *testing.T) {
	releases := &contextReleaseCounter{}
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:              1,
		MinSegmentsPerShard:     1,
		MaxSegmentsPerShard:     1,
		SegmentSize:             1,
		RecordsPerOperation:     1,
		CompletedRingPerShard:   1,
		ReleaseContextVersionFn: releases.release,
	})

	first, ok := manager.StartOperation(1, apiobs.ParentRef{}, 10)
	if !ok {
		t.Fatal("first operation should fit")
	}
	if _, ok := manager.StartOperation(2, apiobs.ParentRef{}, 20); ok {
		t.Fatal("second operation should skip when no slot is free")
	}
	if skipped := manager.SkippedOperations(); skipped != 1 {
		t.Fatalf("SkippedOperations() = %d, want 1", skipped)
	}
	if releases.count(20) != 0 {
		t.Fatalf("no-slot start released version 20 %d times, want 0 because it must not pin", releases.count(20))
	}
	if !manager.FinishOperation(first, SlotTerminalFinished) {
		t.Fatal("finish should enqueue first operation")
	}
	manager.DrainCompletedShard(0, func(CompletedOperation) {})
	if releases.count(10) != 1 {
		t.Fatalf("context releases for first version = %d, want 1", releases.count(10))
	}
}

func TestSlotTrackerDropsRecordsWhenOperationBufferFull(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("operation should fit")
	}
	first := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)
	second := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandFinish, 1)
	if !manager.RecordTelemetry(handle, first) {
		t.Fatal("first record should fit")
	}
	if manager.RecordTelemetry(handle, second) {
		t.Fatal("second record should drop when operation record storage is full")
	}
	if dropped := manager.DroppedRecords(); dropped != 1 {
		t.Fatalf("DroppedRecords() = %d, want 1", dropped)
	}
	if !manager.FinishOperation(handle, SlotTerminalFinished) {
		t.Fatal("finish should enqueue completed operation")
	}
	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		if operation.DroppedRecords != 1 {
			t.Fatalf("completed dropped records = %d, want 1", operation.DroppedRecords)
		}
		if len(operation.Records) != 1 || operation.Records[0].Kind != apiobs.TelemetryRecordCommandStart {
			t.Fatalf("records = %+v, want only first command-start record", operation.Records)
		}
	})
}

func TestSlotTrackerCompletedRingFullDropsAndReleasesContext(t *testing.T) {
	releases := &contextReleaseCounter{}
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:              1,
		MinSegmentsPerShard:     1,
		MaxSegmentsPerShard:     1,
		SegmentSize:             2,
		RecordsPerOperation:     1,
		CompletedRingPerShard:   1,
		ReleaseContextVersionFn: releases.release,
	})
	first, ok := manager.StartOperation(1, apiobs.ParentRef{}, 10)
	if !ok {
		t.Fatal("first operation should fit")
	}
	second, ok := manager.StartOperation(2, apiobs.ParentRef{}, 20)
	if !ok {
		t.Fatal("second operation should fit")
	}
	if !manager.FinishOperation(first, SlotTerminalFinished) {
		t.Fatal("first finish should enqueue")
	}
	if manager.FinishOperation(second, SlotTerminalFailed) {
		t.Fatal("second finish should drop when completed ring is full")
	}
	if dropped := manager.DroppedCompletedOperations(); dropped != 1 {
		t.Fatalf("DroppedCompletedOperations() = %d, want 1", dropped)
	}
	if releases.count(20) != 1 {
		t.Fatalf("dropped completed version releases = %d, want 1", releases.count(20))
	}
	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		if operation.Operation != 1 {
			t.Fatalf("drained operation = %d, want 1", operation.Operation)
		}
	})
	if releases.count(10) != 1 {
		t.Fatalf("drained completed version releases = %d, want 1", releases.count(10))
	}
}

func TestSlotTrackerStaleHandleRejectedAfterSlotReuse(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	oldHandle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("operation should fit")
	}
	if !manager.FinishOperation(oldHandle, SlotTerminalFinished) {
		t.Fatal("finish should enqueue")
	}
	manager.DrainCompletedShard(0, func(CompletedOperation) {})
	newHandle, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("slot should be reusable after drain")
	}
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)
	if manager.RecordTelemetry(oldHandle, record) {
		t.Fatal("old handle should not write into reused slot")
	}
	if !manager.RecordTelemetry(newHandle, apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 2)) {
		t.Fatal("new handle should write into reused slot")
	}
}

func TestSlotTrackerShardStatsReturnsWhileShardMutexHeld(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           2,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})

	manager.shards[0].mu.Lock()
	defer manager.shards[0].mu.Unlock()

	statsCh := make(chan SlotShardStats, 1)
	go func() {
		statsCh <- manager.ShardStats(0)
	}()

	select {
	case stats := <-statsCh:
		if stats.Segments != 1 || stats.FreeSlots != 2 || stats.ActiveSlots != 0 || stats.CompletedSlots != 0 {
			t.Fatalf("ShardStats() = %+v, want segments=1 free=2 active=0 completed=0", stats)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ShardStats blocked while shard mutex was held")
	}
}

func TestSlotTrackerGrowsAndRetiresFreeSegments(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   2,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 2,
	})
	first, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("first operation should fit")
	}
	if _, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0); ok {
		t.Fatal("second operation should not fit before background growth")
	}
	if !manager.GrowShard(0) {
		t.Fatal("background GrowShard should add a free segment")
	}
	second, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("second operation should fit after growth")
	}
	stats := manager.ShardStats(0)
	if stats.Segments != 2 || stats.FreeSlots != 0 || stats.ActiveSlots != 2 {
		t.Fatalf("stats after growth = %+v, want segments=2 free=0 active=2", stats)
	}
	if !manager.FinishOperation(first, SlotTerminalFinished) || !manager.FinishOperation(second, SlotTerminalFinished) {
		t.Fatal("finishes should enqueue")
	}
	manager.DrainCompletedShard(0, func(CompletedOperation) {})
	if !manager.RetireFreeSegment(0) {
		t.Fatal("background RetireFreeSegment should remove one fully free segment")
	}
	stats = manager.ShardStats(0)
	if stats.Segments != 1 || stats.FreeSlots != 1 || stats.ActiveSlots != 0 {
		t.Fatalf("stats after retire = %+v, want segments=1 free=1 active=0", stats)
	}
}

func TestSlotTrackerShardsByConnection(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            4,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           4,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 4,
	})
	conn1 := apiobs.ConnectionIdentity(1)
	conn2 := apiobs.ConnectionIdentity(2)

	h1, _, ok := manager.StartOperationForConnection(1, apiobs.ParentRef{}, conn1)
	if !ok {
		t.Fatal("first conn1 op should start")
	}
	h2, _, ok := manager.StartOperationForConnection(2, apiobs.ParentRef{}, conn1)
	if !ok {
		t.Fatal("second conn1 op should start")
	}
	h3, _, ok := manager.StartOperationForConnection(3, apiobs.ParentRef{}, conn2)
	if !ok {
		t.Fatal("conn2 op should start")
	}

	if h1.shard != h2.shard {
		t.Fatalf("conn1 ops on different shards: %d vs %d", h1.shard, h2.shard)
	}
	if h1.shard == h3.shard {
		t.Fatalf("conn1 and conn2 on same shard: %d", h1.shard)
	}
}

func TestSlotTrackerStartOperationForConnectionPinsInitialContextVersion(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	connection := apiobs.ConnectionIdentity(9)
	v1 := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	handle, pinned, ok := manager.StartOperationForConnection(44, apiobs.ParentRef{}, connection)
	if !ok {
		t.Fatal("operation should fit")
	}
	if pinned != v1 {
		t.Fatalf("pinned version = %d, want initial version %d", pinned, v1)
	}
	v2 := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("globex"))
	if v2 == v1 {
		t.Fatal("context update should create a new version")
	}
	if !manager.FinishOperation(handle, SlotTerminalFinished) {
		t.Fatal("finish should enqueue")
	}
	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		if operation.ContextVersion != v1 {
			t.Fatalf("completed context version = %d, want start-time version %d", operation.ContextVersion, v1)
		}
		got := make(map[string]string)
		if !manager.VisitConnectionContextVersion(operation.ContextVersion, func(key, value string) bool {
			got[key] = value
			return true
		}) {
			t.Fatal("start-time context version should remain visitable during drain")
		}
		if got["tenant"] != "acme" || got["role"] != "reader" {
			t.Fatalf("start-time context = %+v, want tenant=acme role=reader", got)
		}
	})
	if manager.VisitConnectionContextVersion(v1, nil) {
		t.Fatal("non-current start-time version should be released after drain")
	}
	gotCurrent := make(map[string]string)
	if !manager.VisitConnectionContextVersion(v2, func(key, value string) bool {
		gotCurrent[key] = value
		return true
	}) {
		t.Fatal("current version should remain visitable")
	}
	if gotCurrent["tenant"] != "globex" || gotCurrent["role"] != "reader" {
		t.Fatalf("current context = %+v, want tenant=globex role=reader", gotCurrent)
	}
}

func TestSlotTrackerStartOperationWithConnectionContextAttachesCommandOverlay(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	connection := apiobs.ConnectionIdentity(10)
	baseVersion := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	overlay := map[string]string{"tenant": "globex", "traceparent": "00-abc"}

	handle, pinned, ok := manager.StartOperationWithConnectionContext(45, apiobs.ParentRef{}, connection, overlay)
	if !ok {
		t.Fatal("operation should fit")
	}
	if pinned != baseVersion {
		t.Fatalf("pinned version = %d, want base version %d", pinned, baseVersion)
	}
	if !manager.FinishOperation(handle, SlotTerminalFinished) {
		t.Fatal("finish should enqueue")
	}

	var completed CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		completed = cloneCompletedOperation(operation)
	}); drained != 1 {
		t.Fatalf("drained operations = %d, want 1", drained)
	}
	if completed.ContextVersion != baseVersion {
		t.Fatalf("completed context version = %d, want base version %d", completed.ContextVersion, baseVersion)
	}
	if completed.ContextOverlay["tenant"] != "globex" || completed.ContextOverlay["traceparent"] != "00-abc" {
		t.Fatalf("completed context overlay = %+v, want tenant=globex traceparent=00-abc", completed.ContextOverlay)
	}

	current := make(map[string]string)
	if !manager.VisitConnectionContextVersion(baseVersion, func(key, value string) bool {
		current[key] = value
		return true
	}) {
		t.Fatal("current connection context should remain visitable")
	}
	if current["tenant"] != "acme" || current["role"] != "reader" {
		t.Fatalf("current connection context = %+v, want tenant=acme role=reader", current)
	}
	if _, ok := current["traceparent"]; ok {
		t.Fatalf("command overlay leaked into connection context: %+v", current)
	}
}

func TestSlotTrackerForgetConnectionContextKeepsPinnedVersionUntilDrain(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	connection := apiobs.ConnectionIdentity(11)
	v1 := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"))
	handle, pinned, ok := manager.StartOperationForConnection(55, apiobs.ParentRef{}, connection)
	if !ok {
		t.Fatal("operation should fit")
	}
	if pinned != v1 {
		t.Fatalf("pinned version = %d, want %d", pinned, v1)
	}
	if !manager.FinishOperation(handle, SlotTerminalFinished) {
		t.Fatal("finish should enqueue")
	}
	if !manager.ForgetConnectionContext(connection) {
		t.Fatal("forget should remove the current connection version")
	}
	if !manager.VisitConnectionContextVersion(v1, nil) {
		t.Fatal("pinned version should remain visitable after current connection is forgotten")
	}
	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		if operation.ContextVersion != v1 {
			t.Fatalf("completed context version = %d, want %d", operation.ContextVersion, v1)
		}
		if !manager.VisitConnectionContextVersion(operation.ContextVersion, nil) {
			t.Fatal("pinned version should remain visitable during drain")
		}
	})
	if manager.VisitConnectionContextVersion(v1, nil) {
		t.Fatal("forgotten pinned version should be released after drain")
	}
}

func TestSlotTrackerForgetConnectionContextDeletesUnpinnedCurrentVersion(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	connection := apiobs.ConnectionIdentity(12)
	version := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"))
	if version.IsZero() {
		t.Fatal("context version should be non-zero")
	}
	if !manager.ForgetConnectionContext(connection) {
		t.Fatal("forget should remove current connection context")
	}
	if manager.VisitConnectionContextVersion(version, nil) {
		t.Fatal("unpinned forgotten current version should be deleted")
	}
	if manager.ForgetConnectionContext(connection) {
		t.Fatal("forget should report false after current context was removed")
	}
}

func TestSlotTrackerStartOperationForConnectionNoSlotReleasesPinnedVersion(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	if _, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0); !ok {
		t.Fatal("first operation should occupy the only slot")
	}
	connection := apiobs.ConnectionIdentity(3)
	v1 := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"))
	if _, pinned, ok := manager.StartOperationForConnection(2, apiobs.ParentRef{}, connection); ok || pinned != 0 {
		t.Fatalf("no-slot start = ok %v pinned %d, want ok=false pinned=0", ok, pinned)
	}
	manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("globex"))
	if manager.VisitConnectionContextVersion(v1, nil) {
		t.Fatal("no-slot start should release its pinned non-current version")
	}
}

func TestSlotTrackerOperationContextSnapshotFoldsBaseOverlayAndRecords(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   4,
		CompletedRingPerShard: 1,
	})
	connection := apiobs.ConnectionIdentity(13)
	manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	overlay := map[string]string{"tenant": "globex", "traceparent": "00-abc"}
	handle, _, ok := manager.StartOperationWithConnectionContext(77, apiobs.ParentRef{}, connection, overlay)
	if !ok {
		t.Fatal("operation should fit")
	}
	scope := NewOperationScope(manager, handle, 77, apiobs.OperationRef{})
	if !scope.ContextUpdateStrings("role", "writer", "request", "req-1") {
		t.Fatal("context update should fit")
	}
	if !scope.ContextRemoveStrings("traceparent") {
		t.Fatal("context remove should fit")
	}

	got := manager.OperationContextSnapshot(handle)
	if got["tenant"] != "globex" || got["role"] != "writer" || got["request"] != "req-1" {
		t.Fatalf("snapshot = %+v, want tenant=globex role=writer request=req-1", got)
	}
	if _, ok := got["traceparent"]; ok {
		t.Fatalf("snapshot retained removed traceparent: %+v", got)
	}
	got["tenant"] = "mutated"
	again := manager.OperationContextSnapshot(handle)
	if again["tenant"] != "globex" {
		t.Fatalf("snapshot was not caller-owned: %+v", again)
	}
}

func TestSlotTrackerCompletedOperationContextFoldsBaseOverlayAndRecords(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   4,
		CompletedRingPerShard: 1,
	})
	connection := apiobs.ConnectionIdentity(14)
	manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	handle, _, ok := manager.StartOperationWithConnectionContext(88, apiobs.ParentRef{}, connection, map[string]string{"tenant": "globex", "traceparent": "00-abc"})
	if !ok {
		t.Fatal("operation should fit")
	}
	scope := NewOperationScope(manager, handle, 88, apiobs.OperationRef{})
	if !scope.ContextUpdateStrings("role", "writer", "request", "req-1") {
		t.Fatal("context update should fit")
	}
	if !scope.ContextRemoveStrings("traceparent") {
		t.Fatal("context remove should fit")
	}
	if !scope.Finish(SlotTerminalFinished) {
		t.Fatal("finish should enqueue")
	}

	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		got := manager.CompletedOperationContext(operation)
		if got["tenant"] != "globex" || got["role"] != "writer" || got["request"] != "req-1" {
			t.Fatalf("completed context = %+v, want tenant=globex role=writer request=req-1", got)
		}
		if _, ok := got["traceparent"]; ok {
			t.Fatalf("completed context retained removed traceparent: %+v", got)
		}
	})
}

func TestLossCounterSlotExhaustionIncrementsSkippedOperationsAndBenchstats(t *testing.T) {
	const (
		segmentSize = 3
		maxSegments = 2
		slotCount   = segmentSize * maxSegments
	)
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   maxSegments,
		MaxSegmentsPerShard:   maxSegments,
		SegmentSize:           segmentSize,
		RecordsPerOperation:   1,
		CompletedRingPerShard: slotCount,
	})
	handles := make([]InternalTrackerHandle, 0, slotCount)
	for i := 0; i < slotCount; i++ {
		handle, ok := manager.StartOperation(apiobs.InternalOperationIdentity(i+1), apiobs.ParentRef{}, 0)
		if !ok {
			t.Fatalf("StartOperation(%d) ok=false before shard capacity exhausted; allocated=%d want=%d", i+1, len(handles), slotCount)
		}
		handles = append(handles, handle)
	}
	stats := manager.ShardStats(0)
	if stats.FreeSlots != 0 || stats.ActiveSlots != slotCount || stats.CompletedSlots != 0 {
		t.Fatalf("ShardStats() at exhaustion = %+v, want free=0 active=%d completed=0", stats, slotCount)
	}

	if _, ok := manager.StartOperation(99, apiobs.ParentRef{}, 0); ok {
		t.Fatal("StartOperation beyond SegmentSize*MaxSegments returned ok=true, want ok=false")
	}
	if skipped := manager.SkippedOperations(); skipped != 1 {
		t.Fatalf("SkippedOperations() = %d, want 1", skipped)
	}
	if shardSkipped := manager.ShardSkipped(0); shardSkipped != 1 {
		t.Fatalf("ShardSkipped(0) = %d, want 1", shardSkipped)
	}
	bench := trackerSnapshot(manager)
	if got := bench["operation_tracker.skipped_operations"]; got != "1" {
		t.Fatalf("benchstats skipped_operations = %q, want %q", got, "1")
	}
	if got := bench["operation_tracker.dropped_records"]; got != "0" {
		t.Fatalf("benchstats dropped_records = %q, want %q", got, "0")
	}
	if got := bench["operation_tracker.dropped_completed"]; got != "0" {
		t.Fatalf("benchstats dropped_completed = %q, want %q", got, "0")
	}
	if got := bench["operation_tracker.shard_0.skipped"]; got != "1" {
		t.Fatalf("benchstats shard_0.skipped = %q, want %q", got, "1")
	}

	for i, handle := range handles {
		if !manager.FinishOperation(handle, SlotTerminalFinished) {
			t.Fatalf("FinishOperation(handle[%d]) = false, want true", i)
		}
	}
	if drained := manager.DrainCompletedShard(0, func(CompletedOperation) {}); drained != slotCount {
		t.Fatalf("DrainCompletedShard() = %d, want %d", drained, slotCount)
	}
}

func TestLossCounterRecordOverflowIncrementsDroppedRecords(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("operation should allocate the only slot")
	}
	first := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)
	second := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandFinish, 1)
	if !manager.RecordTelemetry(handle, first) {
		t.Fatal("first telemetry record should fit")
	}
	if manager.RecordTelemetry(handle, second) {
		t.Fatal("second telemetry record should be rejected when RecordsPerOperation=1")
	}
	if dropped := manager.DroppedRecords(); dropped != 1 {
		t.Fatalf("DroppedRecords() = %d, want 1", dropped)
	}
	bench := trackerSnapshot(manager)
	if got := bench["operation_tracker.dropped_records"]; got != "1" {
		t.Fatalf("benchstats dropped_records = %q, want %q", got, "1")
	}
	if !manager.FinishOperation(handle, SlotTerminalFinished) {
		t.Fatal("FinishOperation() should enqueue the operation with dropped-record metadata")
	}
	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		if operation.DroppedRecords != 1 {
			t.Fatalf("CompletedOperation.DroppedRecords = %d, want 1", operation.DroppedRecords)
		}
		if len(operation.Records) != 1 || operation.Records[0].Kind != apiobs.TelemetryRecordCommandStart {
			t.Fatalf("completed records = %+v, want only command.start", operation.Records)
		}
	})
}

func TestLossCounterCompletedRingOverflowIncrementsDroppedCompleted(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           2,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	first, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("first operation should fit")
	}
	second, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("second operation should fit before completed-ring saturation")
	}
	if !manager.FinishOperation(first, SlotTerminalFinished) {
		t.Fatal("first FinishOperation() should enqueue")
	}
	if manager.FinishOperation(second, SlotTerminalFailed) {
		t.Fatal("second FinishOperation() should be rejected when CompletedRingPerShard=1 is full")
	}
	if dropped := manager.DroppedCompletedOperations(); dropped != 1 {
		t.Fatalf("DroppedCompletedOperations() = %d, want 1", dropped)
	}
	bench := trackerSnapshot(manager)
	if got := bench["operation_tracker.dropped_completed"]; got != "1" {
		t.Fatalf("benchstats dropped_completed = %q, want %q", got, "1")
	}
	var drained []apiobs.InternalOperationIdentity
	manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		drained = append(drained, operation.Operation)
	})
	if len(drained) != 1 || drained[0] != 1 {
		t.Fatalf("drained operations = %v, want [1]", drained)
	}
}

func TestLossCounterDropStringBatchOverflowMarkerDrainsCompletedRecord(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(42, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("operation should fit")
	}
	scope := NewOperationScope(manager, handle, 42, apiobs.OperationRef{})
	if !scope.DropString("batch_overflow", "overflow_commands", "5") {
		t.Fatal("DropString batch_overflow record should fit")
	}
	if !scope.Finish(SlotTerminalFinished) {
		t.Fatal("Finish() should enqueue operation with drop marker")
	}

	var completed CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		completed = cloneCompletedOperation(operation)
	}); drained != 1 {
		t.Fatalf("DrainCompletedShard() = %d, want 1", drained)
	}
	if completed.Operation != 42 {
		t.Fatalf("CompletedOperation.Operation = %d, want 42", completed.Operation)
	}
	if len(completed.Records) != 1 {
		t.Fatalf("completed record count = %d, want 1", len(completed.Records))
	}
	dropRecord := completed.Records[0]
	if dropRecord.Kind != apiobs.TelemetryRecordDrop {
		t.Fatalf("completed record kind = %s, want %s", dropRecord.Kind, apiobs.TelemetryRecordDrop)
	}
	if got := string(dropRecord.NameBytes()); got != "batch_overflow" {
		t.Fatalf("drop record name = %q, want %q", got, "batch_overflow")
	}
	fields := telemetryRecordFields(dropRecord)
	if fields["overflow_commands"] != "5" {
		t.Fatalf("drop record overflow_commands = %q, want %q", fields["overflow_commands"], "5")
	}
	if len(fields) != 1 {
		t.Fatalf("drop record fields = %+v, want only overflow_commands=5", fields)
	}
}

func TestSlotTrackerAdversarialMaxSlotPressureSkipsAndRecycles(t *testing.T) {
	const slotCount = 64
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           slotCount,
		RecordsPerOperation:   1,
		CompletedRingPerShard: slotCount,
	})

	handles := make([]InternalTrackerHandle, 0, slotCount)
	for operation := 0; operation < slotCount; operation++ {
		handle, ok := manager.StartOperation(apiobs.InternalOperationIdentity(operation+1), apiobs.ParentRef{}, 0)
		if !ok {
			t.Fatalf("StartOperation(%d) ok=false before maximum slot pressure; allocated=%d want=%d", operation+1, len(handles), slotCount)
		}
		handles = append(handles, handle)
	}

	stats := manager.ShardStats(0)
	if stats.FreeSlots != 0 || stats.ActiveSlots != slotCount || stats.CompletedSlots != 0 {
		t.Fatalf("stats at maximum pressure = %+v, want free=0 active=%d completed=0", stats, slotCount)
	}
	if _, ok := manager.StartOperation(10_000, apiobs.ParentRef{}, 0); ok {
		t.Fatal("StartOperation beyond maximum slot pressure returned ok=true, want clean ok=false")
	}
	if skipped := manager.SkippedOperations(); skipped != 1 {
		t.Fatalf("SkippedOperations() after full-shard attack = %d, want 1", skipped)
	}
	if shardSkipped := manager.ShardSkipped(0); shardSkipped != 1 {
		t.Fatalf("ShardSkipped(0) after full-shard attack = %d, want 1", shardSkipped)
	}

	for i, handle := range handles {
		if !manager.FinishOperation(handle, SlotTerminalFinished) {
			t.Fatalf("FinishOperation(handle[%d]) = false, want true", i)
		}
	}
	if drained := manager.DrainCompletedShard(0, func(operation CompletedOperation) {}); drained != slotCount {
		t.Fatalf("DrainCompletedShard drained %d after recycling pressure slots, want %d", drained, slotCount)
	}
	stats = manager.ShardStats(0)
	if stats.FreeSlots != slotCount || stats.ActiveSlots != 0 || stats.CompletedSlots != 0 {
		t.Fatalf("stats after drain/recycle = %+v, want free=%d active=0 completed=0", stats, slotCount)
	}

	recycled := make([]InternalTrackerHandle, 0, slotCount)
	for operation := 0; operation < slotCount; operation++ {
		handle, ok := manager.StartOperation(apiobs.InternalOperationIdentity(20_000+operation), apiobs.ParentRef{}, 0)
		if !ok {
			t.Fatalf("recycled StartOperation(%d) ok=false; allocated=%d want=%d", operation, len(recycled), slotCount)
		}
		recycled = append(recycled, handle)
	}
	for i, stale := range handles {
		if manager.FinishOperation(stale, SlotTerminalFinished) {
			t.Fatalf("stale handle[%d] finished a recycled slot, want generation rejection", i)
		}
	}
	if invalid := manager.InvalidHandles(); invalid != slotCount {
		t.Fatalf("InvalidHandles() after stale reuse attempts = %d, want %d", invalid, slotCount)
	}
	for i, handle := range recycled {
		if !manager.FinishOperation(handle, SlotTerminalFinished) {
			t.Fatalf("FinishOperation(recycled[%d]) = false, want true", i)
		}
	}
	if drained := manager.DrainCompletedShard(0, func(operation CompletedOperation) {}); drained != slotCount {
		t.Fatalf("DrainCompletedShard drained %d recycled operations, want %d", drained, slotCount)
	}
	if skipped := manager.SkippedOperations(); skipped != 1 {
		t.Fatalf("SkippedOperations() changed after recycle/stale attack = %d, want 1", skipped)
	}
	// NOTE: The slot tracker is single-producer-per-handle (slot_tracker.go:49). Concurrent RecordTelemetry on the same handle is a contract violation, not a supported access pattern. If future hardening requires concurrent-same-handle safety, operationSlot.droppedRecords must become atomic.
}
