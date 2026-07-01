package observability

import (
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	apiobs "gocache/api/observability"
)

func TestConnectionContextOwnedPinPreservesPointInTimeVersion(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		CompletedRingPerShard: 1,
	})
	connection := apiobs.ConnectionIdentity(31)
	var owner ConnectionContext

	v1 := manager.UpdateOwnedConnectionContextStrings(&owner, connection, "tenant", "acme", "role", "reader")
	pinned := manager.PinOwnedConnectionContextVersion(&owner)
	if pinned != v1 {
		t.Fatalf("pinned version = %d, want %d", pinned, v1)
	}
	v2 := manager.UpdateOwnedConnectionContextStrings(&owner, connection, "tenant", "globex")
	if v2 == v1 {
		t.Fatal("owned context update should create a new version")
	}

	gotV1 := collectSlotContext(t, manager, pinned)
	if gotV1["tenant"] != "acme" || gotV1["role"] != "reader" {
		t.Fatalf("pinned context = %+v, want tenant=acme role=reader", gotV1)
	}
	gotV2 := collectSlotContext(t, manager, v2)
	if gotV2["tenant"] != "globex" || gotV2["role"] != "reader" {
		t.Fatalf("current context = %+v, want tenant=globex role=reader", gotV2)
	}
	if !manager.ReleaseConnectionContextVersion(pinned) {
		t.Fatal("release of pinned owner version should succeed")
	}
	manager.ReclaimConnectionContextVersions()
	if manager.VisitConnectionContextVersion(v1, nil) {
		t.Fatal("released non-current owner version should be reclaimed")
	}
	if !manager.VisitConnectionContextVersion(v2, nil) {
		t.Fatal("current owner version should remain visitable")
	}
}

func TestConnectionContextPinVisitReleaseRace(t *testing.T) {
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            2,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           8,
		CompletedRingPerShard: 8,
	})
	connection := apiobs.ConnectionIdentity(32)
	var owner ConnectionContext
	if version := manager.UpdateOwnedConnectionContextStrings(&owner, connection, "tenant", "0", "stable", "yes"); version.IsZero() {
		t.Fatal("initial owner context version should be non-zero")
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	failures := make(chan string, 1)
	report := func(message string) {
		select {
		case failures <- message:
		default:
		}
		stop.Store(true)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 2000 && !stop.Load(); i++ {
			manager.UpdateOwnedConnectionContextStrings(&owner, connection, "tenant", strconv.Itoa(i), "stable", "yes")
			if i%3 == 0 {
				manager.RemoveOwnedConnectionContextStrings(&owner, connection, "ephemeral")
			}
			runtime.Gosched()
		}
		stop.Store(true)
	}()

	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				version := manager.PinOwnedConnectionContextVersion(&owner)
				if version.IsZero() {
					runtime.Gosched()
					continue
				}
				visited := manager.VisitConnectionContextVersion(version, func(key, value string) bool {
					if key == "stable" && value != "yes" {
						report("visited pinned context with unstable stable field")
						return false
					}
					return true
				})
				if !visited {
					report("pinned context version was reclaimed before release")
				}
				if !manager.ReleaseConnectionContextVersion(version) {
					report("release of pinned context version failed")
				}
				runtime.Gosched()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			manager.ReclaimConnectionContextVersions()
			runtime.Gosched()
		}
	}()

	wg.Wait()
	select {
	case failure := <-failures:
		t.Fatal(failure)
	default:
	}
}

func collectSlotContext(t *testing.T, manager *SlotOperationTrackerManager, version apiobs.ConnectionContextVersion) map[string]string {
	t.Helper()
	got := make(map[string]string)
	if !manager.VisitConnectionContextVersion(version, func(key, value string) bool {
		got[key] = value
		return true
	}) {
		t.Fatalf("version %d not found", version)
	}
	return got
}
