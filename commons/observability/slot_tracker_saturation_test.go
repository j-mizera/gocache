package observability

import (
	"testing"

	apiobs "gocache/api/observability"
)

func TestSlotTrackerSaturationSkipsPinnedAndNonPinnedConnectionStarts(t *testing.T) {
	tests := []struct {
		name  string
		start func(t *testing.T, manager *SlotOperationTrackerManager) apiobs.ConnectionContextVersion
	}{
		{
			name: "non-pinned connection context",
			start: func(t *testing.T, manager *SlotOperationTrackerManager) apiobs.ConnectionContextVersion {
				t.Helper()
				connection := apiobs.ConnectionIdentity(21)
				oldVersion := manager.UpdateConnectionContextStrings(connection, "tenant", "acme")
				_, pinned, ok := manager.StartOperationWithConnectionContextAndMetadata(
					2,
					apiobs.NewParentRef("parent"),
					connection,
					map[string]string{"traceparent": "00-abc"},
					OperationSnapshotMetadata{Type: "command"},
				)
				if ok || !pinned.IsZero() {
					t.Fatalf("saturated non-pinned start = ok %v pinned %d, want ok=false pinned=0", ok, pinned)
				}
				manager.UpdateConnectionContextStrings(connection, "tenant", "globex")
				return oldVersion
			},
		},
		{
			name: "pinned owner context with magazine",
			start: func(t *testing.T, manager *SlotOperationTrackerManager) apiobs.ConnectionContextVersion {
				t.Helper()
				connection := apiobs.ConnectionIdentity(22)
				var owner ConnectionContext
				var magazine SlotMagazine
				oldVersion := manager.UpdateOwnedConnectionContextStrings(&owner, connection, "tenant", "acme")
				pinned := manager.PinOwnedConnectionContextVersion(&owner)
				if pinned != oldVersion {
					t.Fatalf("pinned owner version = %d, want %d", pinned, oldVersion)
				}
				_, ok := manager.StartOperationWithPinnedConnectionContextAndMetadata(
					2,
					apiobs.NewParentRef("parent"),
					connection,
					pinned,
					map[string]string{"traceparent": "00-abc"},
					OperationSnapshotMetadata{Type: "command"},
					&magazine,
				)
				if ok {
					t.Fatal("saturated pinned start should fail")
				}
				manager.UpdateOwnedConnectionContextStrings(&owner, connection, "tenant", "globex")
				return oldVersion
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newSingleSlotSaturationTracker()
			blocker, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
			if !ok {
				t.Fatal("blocker operation should occupy the only slot")
			}

			skippedBefore := manager.SkippedOperations()
			oldVersion := tt.start(t, manager)
			if skipped := manager.SkippedOperations(); skipped != skippedBefore+1 {
				t.Fatalf("SkippedOperations() = %d, want exactly %d", skipped, skippedBefore+1)
			}
			if drained := manager.DrainCompletedShard(0, func(CompletedOperation) {}); drained != 0 {
				t.Fatalf("saturated failed start drained %d completed operations, want 0", drained)
			}

			manager.ReclaimConnectionContextVersions()
			if manager.VisitConnectionContextVersion(oldVersion, nil) {
				t.Fatalf("saturated start should release old pinned context version %d", oldVersion)
			}

			if !manager.FinishOperation(blocker, SlotTerminalFinished) {
				t.Fatal("blocker finish should enqueue")
			}
			if drained := manager.DrainCompletedShard(0, func(CompletedOperation) {}); drained != 1 {
				t.Fatalf("blocker drain = %d, want 1", drained)
			}
		})
	}
}

func newSingleSlotSaturationTracker() *SlotOperationTrackerManager {
	return NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		CompletedRingPerShard: 1,
	})
}
