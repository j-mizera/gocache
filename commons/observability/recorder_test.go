package observability

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	apiobs "gocache/api/observability"
)

func TestSingleProducerRecorderFIFO(t *testing.T) {
	recorder := newSingleProducerRecorder(8)
	tracker := newOperationTracker(recorder)
	parent := apiobs.NewParentRef("parent-op")
	version := apiobs.ConnectionContextVersion(10)

	if !tracker.StartOperation(1, parent, version) {
		t.Fatal("StartOperation should be accepted")
	}
	if !tracker.StartCommand(1, []byte("GET"), []byte("key:1")) {
		t.Fatal("StartCommand should be accepted")
	}
	if !tracker.FinishCommand(1, 0) {
		t.Fatal("FinishCommand should be accepted")
	}
	if !tracker.FinishOperation(1) {
		t.Fatal("FinishOperation should be accepted")
	}

	var got []apiobs.TelemetryRecord
	recorder.drain(func(record apiobs.TelemetryRecord) {
		got = append(got, record)
	})

	wantKinds := []apiobs.TelemetryRecordKind{
		apiobs.TelemetryRecordOperationStart,
		apiobs.TelemetryRecordCommandStart,
		apiobs.TelemetryRecordCommandFinish,
		apiobs.TelemetryRecordOperationFinish,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("drained %d records, want %d", len(got), len(wantKinds))
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Fatalf("record[%d].Kind = %v, want %v", i, got[i].Kind, want)
		}
	}
	if got[0].ContextVersion != version {
		t.Fatalf("operation start context version = %d, want %d", got[0].ContextVersion, version)
	}
	if got[0].Parent.String() != "parent-op" {
		t.Fatalf("operation parent = %q, want parent-op", got[0].Parent.String())
	}
	if string(got[1].NameBytes()) != "GET" {
		t.Fatalf("command name = %q, want GET", got[1].NameBytes())
	}
	if string(got[1].PayloadBytes()) != "key:1" {
		t.Fatalf("command payload = %q, want key:1", got[1].PayloadBytes())
	}
}

func TestSingleProducerRecorderDropsWhenFull(t *testing.T) {
	recorder := NewSingleProducerTelemetryRecorder(1)
	if !recorder.RecordTelemetry(apiobs.NewTelemetryRecord(apiobs.TelemetryRecordEvent, 1)) {
		t.Fatal("first record should fit")
	}
	if recorder.RecordTelemetry(apiobs.NewTelemetryRecord(apiobs.TelemetryRecordEvent, 2)) {
		t.Fatal("second record should drop when capacity is one")
	}
	if dropped := recorder.DroppedRecords(); dropped != 1 {
		t.Fatalf("DroppedRecords() = %d, want 1", dropped)
	}
}

func TestShardIndexRouteStableByIdentity(t *testing.T) {
	for identity := apiobs.InternalOperationIdentity(1); identity < 32; identity++ {
		first := shardIndex(identity, 8)
		for range 10 {
			if got := shardIndex(identity, 8); got != first {
				t.Fatalf("shardIndex(%d, 8) changed from %d to %d", identity, first, got)
			}
		}
	}
	if got := shardIndex(99, 0); got != 0 {
		t.Fatalf("shardIndex with zero shards = %d, want 0", got)
	}
}

func TestTelemetryTrackerCopiesNamePayloadAndFields(t *testing.T) {
	recorder := newSingleProducerRecorder(4)
	tracker := newOperationTracker(recorder)
	message := []byte("original-message")
	key := []byte("status")
	value := []byte("ok")
	if !tracker.Log(1, apiobs.TelemetryLogLevelDebug, message, key, value) {
		t.Fatal("Log should be accepted")
	}
	copy(message, "mutated-message")
	copy(value, "no")

	var got apiobs.TelemetryRecord
	if !recorder.ring.pop(&got) {
		t.Fatal("expected one record")
	}
	if string(got.NameBytes()) != "original-message" {
		t.Fatalf("log message = %q, want original-message", got.NameBytes())
	}
	if got.Level != apiobs.TelemetryLogLevelDebug {
		t.Fatalf("log level = %v, want debug", got.Level)
	}
	if got.Payload[0] != 1 {
		t.Fatalf("packed field count = %d, want 1", got.Payload[0])
	}
}

func TestTelemetryTrackerPacksEventAndContextMutations(t *testing.T) {
	recorder := newSingleProducerRecorder(4)
	tracker := newOperationTracker(recorder)
	if !tracker.Event(1, []byte("custom.plugin.event"), []byte("tenant"), []byte("acme")) {
		t.Fatal("Event should be accepted")
	}
	if !tracker.ContextUpdate(1, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader")) {
		t.Fatal("ContextUpdate should be accepted")
	}
	if !tracker.ContextRemove(1, []byte("role")) {
		t.Fatal("ContextRemove should be accepted")
	}

	var event apiobs.TelemetryRecord
	if !recorder.ring.pop(&event) {
		t.Fatal("expected event record")
	}
	if event.Kind != apiobs.TelemetryRecordEvent {
		t.Fatalf("kind = %v, want event", event.Kind)
	}
	if string(event.NameBytes()) != "custom.plugin.event" {
		t.Fatalf("event name = %q, want custom.plugin.event", event.NameBytes())
	}
	if event.Payload[0] != 1 {
		t.Fatalf("event field count = %d, want 1", event.Payload[0])
	}

	var update apiobs.TelemetryRecord
	if !recorder.ring.pop(&update) {
		t.Fatal("expected update record")
	}
	if update.Kind != apiobs.TelemetryRecordContextUpdate {
		t.Fatalf("kind = %v, want context update", update.Kind)
	}
	if update.Payload[0] != 2 {
		t.Fatalf("packed pair count = %d, want 2", update.Payload[0])
	}

	var remove apiobs.TelemetryRecord
	if !recorder.ring.pop(&remove) {
		t.Fatal("expected remove record")
	}
	if remove.Kind != apiobs.TelemetryRecordContextRemove {
		t.Fatalf("kind = %v, want context remove", remove.Kind)
	}
	if remove.Payload[0] != 1 {
		t.Fatalf("packed remove count = %d, want 1", remove.Payload[0])
	}
}

func TestTelemetryTrackerContextMutationRecordsDrainBeforeLaterLogs(t *testing.T) {
	recorder := newSingleProducerRecorder(8)
	tracker := newOperationTracker(recorder)

	if !tracker.ContextUpdate(7, []byte("tenant"), []byte("acme")) {
		t.Fatal("ContextUpdate should be accepted")
	}
	if !tracker.Log(7, apiobs.TelemetryLogLevelInfo, []byte("tenant active")) {
		t.Fatal("first Log should be accepted")
	}
	if !tracker.ContextRemove(7, []byte("tenant")) {
		t.Fatal("ContextRemove should be accepted")
	}
	if !tracker.Log(7, apiobs.TelemetryLogLevelInfo, []byte("tenant removed")) {
		t.Fatal("second Log should be accepted")
	}

	var got []apiobs.TelemetryRecordKind
	recorder.drain(func(record apiobs.TelemetryRecord) {
		got = append(got, record.Kind)
	})

	want := []apiobs.TelemetryRecordKind{
		apiobs.TelemetryRecordContextUpdate,
		apiobs.TelemetryRecordLog,
		apiobs.TelemetryRecordContextRemove,
		apiobs.TelemetryRecordLog,
	}
	if len(got) != len(want) {
		t.Fatalf("drained %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record[%d].Kind = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestShardedManagerRoutesPrecreatedTrackersByShard(t *testing.T) {
	manager := NewShardedOperationTrackerManager(4, 8)
	first := manager.Get(1)
	sameShard := manager.Get(5)
	differentShard := manager.Get(2)
	if first != sameShard {
		t.Fatal("operations 1 and 5 should route to the same pre-created shard tracker")
	}
	if first == differentShard {
		t.Fatal("operations 1 and 2 should route to different shard trackers")
	}
	if manager.ShardCount() != 4 {
		t.Fatalf("ShardCount() = %d, want 4", manager.ShardCount())
	}
}

func TestShardedManagerDrainsByShard(t *testing.T) {
	manager := NewShardedOperationTrackerManager(4, 8)
	tracker := manager.Get(5)
	if !tracker.StartCommand(5, []byte("PLUGIN.CMD"), []byte("arg")) {
		t.Fatal("StartCommand should be accepted")
	}
	var got []apiobs.TelemetryRecord
	if drained := manager.DrainShard(1, func(record apiobs.TelemetryRecord) {
		got = append(got, record)
	}); drained != 1 {
		t.Fatalf("DrainShard drained %d records, want 1", drained)
	}
	if len(got) != 1 || got[0].Operation != 5 {
		t.Fatalf("drained records = %+v, want operation 5", got)
	}
}

func TestConnectionContextVersionsArePinnedImmutable(t *testing.T) {
	manager := NewShardedOperationTrackerManager(2, 8)
	conn := apiobs.ConnectionIdentity(7)
	v1 := manager.UpdateConnectionContext(conn, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	pinned := manager.PinCurrentConnectionContextVersion(conn)
	if pinned != v1 {
		t.Fatalf("pinned version = %d, want %d", pinned, v1)
	}
	v2 := manager.UpdateConnectionContext(conn, []byte("tenant"), []byte("globex"))
	if v2 == v1 {
		t.Fatal("context update should create a new version")
	}

	gotV1 := collectContext(t, manager, v1)
	if gotV1["tenant"] != "acme" || gotV1["role"] != "reader" {
		t.Fatalf("v1 context = %+v, want tenant=acme role=reader", gotV1)
	}
	gotV2 := collectContext(t, manager, v2)
	if gotV2["tenant"] != "globex" || gotV2["role"] != "reader" {
		t.Fatalf("v2 context = %+v, want tenant=globex role=reader", gotV2)
	}
	if !manager.ReleaseConnectionContextVersion(v1) {
		t.Fatal("release of pinned v1 should succeed")
	}
	if manager.VisitConnectionContextVersion(v1, nil) {
		t.Fatal("released non-current v1 should be reclaimed")
	}
}

func collectContext(t *testing.T, manager *ShardedOperationTrackerManager, version apiobs.ConnectionContextVersion) map[string]string {
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

func TestShardedManagerMultiProducerRace(t *testing.T) {
	const producers = 8
	const perProducer = 128
	const total = producers * perProducer
	manager := NewShardedOperationTrackerManager(1, total*2)
	tracker := manager.Get(1)

	var wg sync.WaitGroup
	wg.Add(producers)
	for producer := range producers {
		producer := producer
		go func() {
			defer wg.Done()
			for i := range perProducer {
				operation := apiobs.InternalOperationIdentity(producer*perProducer + i + 1)
				for !tracker.StartCommand(operation, []byte("PING"), nil) {
					runtime.Gosched()
				}
			}
		}()
	}
	wg.Wait()

	var drained int64
	manager.DrainShard(0, func(apiobs.TelemetryRecord) {
		atomic.AddInt64(&drained, 1)
	})
	if got := atomic.LoadInt64(&drained); got != total {
		t.Fatalf("drained %d records, want %d", got, total)
	}
}
