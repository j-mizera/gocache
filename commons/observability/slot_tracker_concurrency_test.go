package observability

import (
	"sync"
	"testing"

	apiobs "gocache/api/observability"
)

func TestSlotTrackerDoubleFinishRejectedAndContextReleasedOnce(t *testing.T) {
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

	version := apiobs.ConnectionContextVersion(42)
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, version)
	if !ok {
		t.Fatal("operation should fit")
	}
	if !manager.FinishOperation(handle, SlotTerminalFinished) {
		t.Fatal("first finish should transition active slot")
	}
	if manager.FinishOperation(handle, SlotTerminalFailed) {
		t.Fatal("second finish should be rejected for terminal slot")
	}
	if manager.InvalidHandles() == 0 {
		t.Fatal("double finish should increment invalid handle counter")
	}
	if releases.count(version) != 0 {
		t.Fatalf("context released before drain = %d, want 0", releases.count(version))
	}
	if drained := manager.DrainCompletedShard(0, func(CompletedOperation) {}); drained != 1 {
		t.Fatalf("drained = %d, want 1", drained)
	}
	if releases.count(version) != 1 {
		t.Fatalf("context releases = %d, want 1", releases.count(version))
	}
}

func TestSlotTrackerConcurrentIndependentOperations(t *testing.T) {
	const operations = 128
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            8,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           operations,
		RecordsPerOperation:   2,
		CompletedRingPerShard: operations,
	})

	var wg sync.WaitGroup
	for i := 0; i < operations; i++ {
		operation := apiobs.InternalOperationIdentity(i + 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle, ok := manager.StartOperation(operation, apiobs.ParentRef{}, 0)
			if !ok {
				t.Errorf("StartOperation(%d) skipped with ample capacity", operation)
				return
			}
			if !manager.RecordTelemetry(handle, apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, operation)) {
				t.Errorf("RecordTelemetry(%d) failed", operation)
			}
			if !manager.FinishOperation(handle, SlotTerminalFinished) {
				t.Errorf("FinishOperation(%d) failed", operation)
			}
		}()
	}
	wg.Wait()

	drained := 0
	for shard := 0; shard < manager.ShardCount(); shard++ {
		drained += manager.DrainCompletedShard(shard, func(operation CompletedOperation) {
			if len(operation.Records) != 1 {
				t.Errorf("operation %d records = %d, want 1", operation.Operation, len(operation.Records))
			}
		})
	}
	if drained != operations {
		t.Fatalf("drained operations = %d, want %d", drained, operations)
	}
	if skipped := manager.SkippedOperations(); skipped != 0 {
		t.Fatalf("skipped operations = %d, want 0", skipped)
	}
	if invalid := manager.InvalidHandles(); invalid != 0 {
		t.Fatalf("invalid handles = %d, want 0", invalid)
	}
}

func TestSlotTrackerRetireFreeSegmentDoesNotInvalidateActiveSegment(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   2,
		SegmentSize:           1,
		RecordsPerOperation:   2,
		CompletedRingPerShard: 2,
	})

	first, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("first operation should fit")
	}
	if !manager.GrowShard(0) {
		t.Fatal("GrowShard should add a second segment")
	}
	second, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("second operation should fit after growth")
	}
	if !manager.FinishOperation(first, SlotTerminalFinished) {
		t.Fatal("first finish should enqueue")
	}
	manager.DrainCompletedShard(0, func(CompletedOperation) {})

	if !manager.RetireFreeSegment(0) {
		t.Fatal("RetireFreeSegment should remove the fully free segment")
	}
	if !manager.RecordTelemetry(second, apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 2)) {
		t.Fatal("active handle in remaining segment should stay valid after retire")
	}
	if !manager.FinishOperation(second, SlotTerminalFinished) {
		t.Fatal("active handle should still finish after retire")
	}
	if drained := manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		if operation.Operation != 2 {
			t.Fatalf("drained operation = %d, want 2", operation.Operation)
		}
	}); drained != 1 {
		t.Fatalf("drained after retire = %d, want 1", drained)
	}
}
