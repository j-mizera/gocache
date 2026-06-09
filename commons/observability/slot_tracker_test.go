package observability

import (
	"testing"

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
