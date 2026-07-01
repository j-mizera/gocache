package server

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apicommand "gocache/api/command"
	apievents "gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"
	apiobs "gocache/api/observability"
	"gocache/commons/logger"
	commonobs "gocache/commons/observability"
)

func TestOperationTrackerDrainWorkerDrainOnceMaterializesLogRequest(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 2, 2)
	handle, ok := manager.StartOperation(1, apiobs.NewParentRef("connection-op"), 0)
	if !ok {
		t.Fatal("StartOperation should allocate a slot")
	}
	record := apiobs.NewLogRecordBytes(0, apiobs.TelemetryLogLevelWarn, []byte("completed command log"))
	if !record.AddFieldBytes([]byte("key"), []byte("value")) {
		t.Fatal("AddFieldBytes should fit test field")
	}
	if !manager.RecordTelemetry(handle, record) {
		t.Fatal("RecordTelemetry should accept log request")
	}
	if !manager.FinishOperation(handle, commonobs.SlotTerminalFinished) {
		t.Fatal("FinishOperation should enqueue completed operation")
	}

	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(&recordingEmitter{subscribed: true})
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want 1", drained)
	}

	entry := decodeSingleLogEntry(t, out.String())
	if got := entry["message"]; got != "completed command log" {
		t.Fatalf("message = %v, want completed command log", got)
	}
	if got := entry["level"]; got != "warn" {
		t.Fatalf("level = %v, want warn", got)
	}
	if got := entry[operationTrackerDrainParentField]; got != "connection-op" {
		t.Fatalf("parent field = %v, want connection-op", got)
	}
	if got := entry["key"]; got != "value" {
		t.Fatalf("log field = %v, want value", got)
	}
	stats := manager.ShardStats(0)
	if stats.CompletedSlots != 0 || stats.ActiveSlots != 0 || stats.FreeSlots != 2 {
		t.Fatalf("stats after drain = %+v, want all slots free", stats)
	}
}

func TestOperationTrackerDrainWorkerMaterializesPinnedBaseContext(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 1, 1)
	connection := apiobs.ConnectionIdentity(21)
	v1 := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	handle, pinned, ok := manager.StartOperationForConnection(1, apiobs.NewParentRef("connection-op"), connection)
	if !ok {
		t.Fatal("StartOperationForConnection should allocate a slot")
	}
	if pinned != v1 {
		t.Fatalf("pinned context version = %d, want %d", pinned, v1)
	}
	v2 := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("globex"))
	scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef("command-op", "connection-op"))
	if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("base context log")) {
		t.Fatal("Log should be accepted")
	}
	if !scope.Finish(commonobs.SlotTerminalFinished) {
		t.Fatal("Finish should enqueue completed operation")
	}

	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(&recordingEmitter{subscribed: true})
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want 1", drained)
	}

	entry := decodeSingleLogEntry(t, out.String())
	ctx := logContext(t, entry)
	if ctx["tenant"] != "acme" || ctx["role"] != "reader" {
		t.Fatalf("materialized context = %+v, want start-time tenant=acme role=reader", ctx)
	}
	if manager.VisitConnectionContextVersion(v1, nil) {
		t.Fatal("non-current pinned base context should be released after drain")
	}
	if !manager.VisitConnectionContextVersion(v2, nil) {
		t.Fatal("current context version should remain visitable after prior pinned version release")
	}
}

func TestOperationTrackerDrainWorkerMaterializesCommandOverlayContext(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 1, 1)
	connection := apiobs.ConnectionIdentity(24)
	baseVersion := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	overlay := map[string]string{"tenant": "globex", "traceparent": "00-abc"}
	handle, pinned, ok := manager.StartOperationWithConnectionContext(1, apiobs.NewParentRef("connection-op"), connection, overlay)
	if !ok {
		t.Fatal("StartOperationWithConnectionContext should allocate a slot")
	}
	if pinned != baseVersion {
		t.Fatalf("pinned context version = %d, want %d", pinned, baseVersion)
	}
	scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef("command-op", "connection-op"))
	if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("command overlay context log")) {
		t.Fatal("Log should be accepted")
	}
	if !scope.Finish(commonobs.SlotTerminalFinished) {
		t.Fatal("Finish should enqueue completed operation")
	}

	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(&recordingEmitter{subscribed: true})
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want 1", drained)
	}

	entry := decodeSingleLogEntry(t, out.String())
	ctx := logContext(t, entry)
	if ctx["tenant"] != "globex" || ctx["role"] != "reader" || ctx["traceparent"] != "00-abc" {
		t.Fatalf("materialized context = %+v, want tenant=globex role=reader traceparent=00-abc", ctx)
	}
	current := map[string]string{}
	if !manager.VisitConnectionContextVersion(baseVersion, func(key, value string) bool {
		current[key] = value
		return true
	}) {
		t.Fatal("current connection context should remain visitable")
	}
	if current["tenant"] != "acme" || current["role"] != "reader" {
		t.Fatalf("current context = %+v, want tenant=acme role=reader", current)
	}
	if _, ok := current["traceparent"]; ok {
		t.Fatalf("command overlay leaked into connection context: %+v", current)
	}
}

func TestOperationTrackerDrainWorkerFoldsContextMutationsInRecordOrder(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 1, 5)
	connection := apiobs.ConnectionIdentity(22)
	manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	handle, _, ok := manager.StartOperationForConnection(1, apiobs.ParentRef{}, connection)
	if !ok {
		t.Fatal("StartOperationForConnection should allocate a slot")
	}
	scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef("command-op", ""))
	if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("before update")) {
		t.Fatal("first Log should be accepted")
	}
	if !scope.ContextUpdate([]byte("tenant"), []byte("globex"), []byte("trace"), []byte("abc")) {
		t.Fatal("ContextUpdate should be accepted")
	}
	if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("after update")) {
		t.Fatal("second Log should be accepted")
	}
	if !scope.ContextRemove([]byte("role")) {
		t.Fatal("ContextRemove should be accepted")
	}
	if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("after remove")) {
		t.Fatal("third Log should be accepted")
	}
	if !scope.Finish(commonobs.SlotTerminalFinished) {
		t.Fatal("Finish should enqueue completed operation")
	}

	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(&recordingEmitter{subscribed: true})
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want 1", drained)
	}

	entries := decodeLogEntries(t, out.String())
	if len(entries) != 3 {
		t.Fatalf("materialized log entries = %d, want 3: %q", len(entries), out.String())
	}
	before := logContext(t, entries[0])
	if entries[0]["message"] != "before update" || before["tenant"] != "acme" || before["role"] != "reader" {
		t.Fatalf("before update entry/context = %+v / %+v", entries[0], before)
	}
	updated := logContext(t, entries[1])
	if entries[1]["message"] != "after update" || updated["tenant"] != "globex" || updated["role"] != "reader" || updated["trace"] != "abc" {
		t.Fatalf("after update entry/context = %+v / %+v", entries[1], updated)
	}
	removed := logContext(t, entries[2])
	if entries[2]["message"] != "after remove" || removed["tenant"] != "globex" || removed["trace"] != "abc" {
		t.Fatalf("after remove entry/context = %+v / %+v", entries[2], removed)
	}
	if _, ok := removed["role"]; ok {
		t.Fatalf("role should be absent after context.remove: %+v", removed)
	}
}

func TestOperationTrackerDrainWorkerRedactsSecretContextAtProjection(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 1, 3)
	connection := apiobs.ConnectionIdentity(23)
	manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme"), []byte("oauth.secret.jwt"), []byte("base-token"))
	handle, _, ok := manager.StartOperationForConnection(1, apiobs.ParentRef{}, connection)
	if !ok {
		t.Fatal("StartOperationForConnection should allocate a slot")
	}
	scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef("command-op", ""))
	if !scope.ContextUpdate([]byte("shared.secret.jwt"), []byte("updated-token"), []byte("trace"), []byte("abc")) {
		t.Fatal("ContextUpdate should be accepted")
	}
	if !scope.Log(apiobs.TelemetryLogLevelInfo, []byte("redacted context")) {
		t.Fatal("Log should be accepted")
	}
	if !scope.Finish(commonobs.SlotTerminalFinished) {
		t.Fatal("Finish should enqueue completed operation")
	}

	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(&recordingEmitter{subscribed: true})
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want 1", drained)
	}

	entry := decodeSingleLogEntry(t, out.String())
	ctx := logContext(t, entry)
	if ctx["tenant"] != "acme" || ctx["trace"] != "abc" {
		t.Fatalf("visible context = %+v, want tenant=acme trace=abc", ctx)
	}
	for _, key := range []string{"oauth.secret.jwt", "shared.secret.jwt"} {
		if _, ok := ctx[key]; ok {
			t.Fatalf("secret key %q should be redacted from local log _ctx: %+v", key, ctx)
		}
	}
}

func TestIsTelemetryVisibleKeyAllowsPublicConnectionMetadata(t *testing.T) {
	testCases := []struct {
		name    string
		key     string
		visible bool
	}{
		{name: "connection id", key: "_connection_id", visible: true},
		{name: "remote address", key: "_remote_addr", visible: true},
		{name: "password", key: "_password", visible: false},
		{name: "shared secret jwt", key: "shared.secret.jwt", visible: false},
		{name: "auth secret api key", key: "auth.secret.api_key", visible: false},
		{name: "secret api key", key: "secret.api_key", visible: false},
		{name: "shared rex data", key: "shared.rex.data", visible: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if visible := isTelemetryVisibleKey(testCase.key); visible != testCase.visible {
				t.Fatalf("isTelemetryVisibleKey(%q) = %v, want %v", testCase.key, visible, testCase.visible)
			}
		})
	}
}

func TestOperationTrackerDrainWorkerSkipsLocalMaterializedLogRequest(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 1, 1)
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate a slot")
	}
	record := apiobs.NewLogRecordBytes(0, apiobs.TelemetryLogLevelInfo, []byte("already written"))
	record.Flags |= apiobs.TelemetryRecordFlagLocalLogMaterialized
	if !manager.RecordTelemetry(handle, record) {
		t.Fatal("RecordTelemetry should accept log request")
	}
	if !manager.FinishOperation(handle, commonobs.SlotTerminalFinished) {
		t.Fatal("FinishOperation should enqueue completed operation")
	}

	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want 1", drained)
	}
	if out.Len() != 0 {
		t.Fatalf("local-materialized record wrote %q, want no duplicate local log", out.String())
	}
	if stats := manager.ShardStats(0); stats.FreeSlots != 1 || stats.CompletedSlots != 0 {
		t.Fatalf("stats after drain = %+v, want completed slot released", stats)
	}
}

func TestOperationTrackerDrainWorkerMaterializesSupportedTelemetryRecords(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 1, 17)
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate a slot")
	}
	scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef("cmd-test", ""))
	if !scope.OperationStartString("command", apicommand.OperationID, "cmd-test") {
		t.Fatal("operation start record should fit")
	}
	if !scope.CommandStartString("PING", apicommand.OperationID, "cmd-test") {
		t.Fatal("command start record should fit")
	}
	if !scope.CommandFinishString("PING", 123, apicommand.OperationID, "cmd-test", apicommand.ResultKey, "PONG") {
		t.Fatal("command finish record should fit")
	}
	if !scope.EventString(string(apievents.ConnectionOpen), apicommand.OperationID, "conn-start", apicommand.RemoteAddrKey, "127.0.0.1:1234", apicommand.ConnectionIDKey, "conn_1") {
		t.Fatal("event record should fit")
	}
	if !scope.EventString(string(apievents.AuthFailed), apicommand.OperationID, "auth-failed", apicommand.RemoteAddrKey, "127.0.0.1:1234", apicommand.CommandKey, "PING") {
		t.Fatal("auth event record should fit")
	}
	if !scope.EventString(string(apievents.ServerShutdown), apicommand.OperationID, "shutdown", "_reason", "signal") {
		t.Fatal("shutdown event record should fit")
	}
	if !scope.EventString(string(apievents.PluginRegistered), apicommand.OperationID, "plugin-registered", apicommand.PluginNameKey, "pubsub", "_version", "0.1.0", "_critical", "false") {
		t.Fatal("plugin registered event record should fit")
	}
	if !scope.EventString(string(apievents.PluginCrashed), apicommand.OperationID, "plugin-crashed", apicommand.PluginNameKey, "pubsub", "_critical", "true", apicommand.ErrorKey, "boom") {
		t.Fatal("plugin crashed event record should fit")
	}
	if !scope.EventString(string(apievents.PluginRestarted), apicommand.OperationID, "plugin-restarted", apicommand.PluginNameKey, "pubsub", "_critical", "false", "_restart_count", "2") {
		t.Fatal("plugin restarted event record should fit")
	}
	if !scope.EventString(string(apievents.PluginStarted), apicommand.OperationID, "plugin-started", apicommand.PluginNameKey, "pubsub", "_critical", "false", "_pid", "4242") {
		t.Fatal("plugin started event record should fit")
	}
	if !scope.EventString(string(apievents.PluginStopped), apicommand.OperationID, "plugin-stopped", apicommand.PluginNameKey, "pubsub", "_critical", "false", "_reason", "shutdown ack") {
		t.Fatal("plugin stopped event record should fit")
	}
	if !scope.EventString(string(apievents.PluginRegistrationFailed), apicommand.OperationID, "plugin-registration-failed", apicommand.PluginNameKey, "pubsub", "_version", "0.1.0", "_critical", "true", apicommand.ErrorKey, "bad register") {
		t.Fatal("plugin registration failed event record should fit")
	}
	if !scope.EventString(string(apievents.PluginCommandRegistered), apicommand.OperationID, "plugin-command-registered", apicommand.PluginNameKey, "pubsub", apicommand.CommandKey, "PUBLISH", "_namespaced", "false", "_readonly", "false") {
		t.Fatal("plugin command registered event record should fit")
	}
	if !scope.EventString(string(apievents.PluginCommandRegistrationFailed), apicommand.OperationID, "plugin-command-registration-failed", apicommand.PluginNameKey, "pubsub", apicommand.CommandKey, "PUBLISH", apicommand.ErrorKey, "shadow core command") {
		t.Fatal("plugin command registration failed event record should fit")
	}
	if !scope.EventString(string(apievents.ConfigReloaded), apicommand.OperationID, "config-reloaded", apicommand.FileKey, "gocache.yaml") {
		t.Fatal("config reloaded event record should fit")
	}
	if !scope.EventString(string(apievents.CacheEviction), apicommand.OperationID, "cache-eviction", "_key", "foo", "_reason", "maxmemory") {
		t.Fatal("cache eviction event record should fit")
	}
	if !scope.DropString("dropped telemetry", "reason", "full") {
		t.Fatal("drop record should fit")
	}
	if !scope.Finish(commonobs.SlotTerminalFinished) {
		t.Fatal("Finish should enqueue completed operation")
	}

	emitter := &recordingEmitter{subscribed: true}
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(emitter)
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want 1", drained)
	}

	gotTypes := make([]string, len(emitter.events))
	for i, event := range emitter.events {
		gotTypes[i] = event.Proto.Type
	}
	wantTypes := []string{
		string(apievents.OperationStarted),
		string(apievents.CommandStarted),
		string(apievents.CommandCompleted),
		string(apievents.ConnectionOpen),
		string(apievents.AuthFailed),
		string(apievents.ServerShutdown),
		string(apievents.PluginRegistered),
		string(apievents.PluginCrashed),
		string(apievents.PluginRestarted),
		string(apievents.PluginStarted),
		string(apievents.PluginStopped),
		string(apievents.PluginRegistrationFailed),
		string(apievents.PluginCommandRegistered),
		string(apievents.PluginCommandRegistrationFailed),
		string(apievents.ConfigReloaded),
		string(apievents.CacheEviction),
	}
	if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	entry := decodeSingleLogEntry(t, out.String())
	if got := entry["message"]; got != "dropped telemetry" {
		t.Fatalf("drop log message = %v, want dropped telemetry", got)
	}
	if stats := manager.ShardStats(0); stats.FreeSlots != 1 || stats.CompletedSlots != 0 {
		t.Fatalf("stats after telemetry drain = %+v, want completed slot released", stats)
	}
}

func TestOperationTrackerDrainWorkerContextCancelPerformsFinalDrain(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 2, 1)
	sentinel, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate sentinel slot")
	}
	if !manager.RecordTelemetry(sentinel, apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)) {
		t.Fatal("RecordTelemetry should accept sentinel record")
	}
	if !manager.FinishOperation(sentinel, commonobs.SlotTerminalFinished) {
		t.Fatal("FinishOperation should enqueue sentinel")
	}

	ctx, cancel := context.WithCancel(context.Background())
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(&recordingEmitter{subscribed: true})
	worker.Start(ctx)
	waitForShardFreeSlots(t, manager, 2)

	handle, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate post-start slot")
	}
	if !manager.RecordTelemetry(handle, apiobs.NewLogRecordBytes(0, apiobs.TelemetryLogLevelInfo, []byte("context cancel drain log"))) {
		t.Fatal("RecordTelemetry should accept log request")
	}
	if !manager.FinishOperation(handle, commonobs.SlotTerminalFinished) {
		t.Fatal("FinishOperation should enqueue post-start operation")
	}

	cancel()
	waitForWorkerExit(t, worker)
	worker.Stop()

	entry := decodeSingleLogEntry(t, out.String())
	if got := entry["message"]; got != "context cancel drain log" {
		t.Fatalf("message = %v, want context cancel drain log", got)
	}
	if stats := manager.ShardStats(0); stats.FreeSlots != 2 || stats.CompletedSlots != 0 {
		t.Fatalf("stats after context-cancel drain = %+v, want completed slot released", stats)
	}
}

func TestOperationTrackerDrainWorkerStopPerformsFinalDrain(t *testing.T) {
	var out bytes.Buffer
	logger.InitWithWriter(&out, "debug")

	manager := newTestSlotOperationTrackerManager(t, 1, 1)
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(&recordingEmitter{subscribed: true})
	worker.Start(context.Background())

	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate a slot")
	}
	if !manager.RecordTelemetry(handle, apiobs.NewLogRecordBytes(0, apiobs.TelemetryLogLevelInfo, []byte("stop drain log"))) {
		t.Fatal("RecordTelemetry should accept log request")
	}
	if !manager.FinishOperation(handle, commonobs.SlotTerminalFinished) {
		t.Fatal("FinishOperation should enqueue completed operation")
	}

	worker.Stop()
	worker.Stop()

	entry := decodeSingleLogEntry(t, out.String())
	if got := entry["message"]; got != "stop drain log" {
		t.Fatalf("message = %v, want stop drain log", got)
	}
	if stats := manager.ShardStats(0); stats.FreeSlots != 1 || stats.CompletedSlots != 0 {
		t.Fatalf("stats after stop drain = %+v, want completed slot released", stats)
	}
}

func TestDrainWorkerParkWake(t *testing.T) {
	manager := newTestSlotOperationTrackerManager(t, 2, 1)
	sentinel, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate sentinel slot")
	}
	if !manager.FinishOperation(sentinel, commonobs.SlotTerminalFinished) {
		t.Fatal("FinishOperation should enqueue sentinel")
	}

	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	// Keep the safety timeout beyond the test deadline so the second operation is
	// drained because FinishOperation nudges the parked worker, not due to polling.
	worker.idleBackoff = time.Hour
	worker.Start(context.Background())
	defer worker.Stop()

	waitForShardFreeSlots(t, manager, 2)
	time.Sleep(5 * time.Millisecond)

	handle, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate post-park slot")
	}
	if !manager.FinishOperation(handle, commonobs.SlotTerminalFinished) {
		t.Fatal("FinishOperation should nudge the parked drain worker")
	}

	waitForShardFreeSlots(t, manager, 2)
	if stats := manager.ShardStats(0); stats.CompletedSlots != 0 || stats.ActiveSlots != 0 {
		t.Fatalf("stats after park/wake drain = %+v, want no active or completed slots", stats)
	}
}

func TestOperationTrackerDrainSerialParityWorkerCountZeroAndOne(t *testing.T) {
	const shards = 8
	zeroIDs, expectedByShard := drainSerialParityOperationIDs(t, 0, shards)
	oneIDs, _ := drainSerialParityOperationIDs(t, 1, shards)

	assertShardFIFOOrder(t, zeroIDs, expectedByShard, shards)
	assertShardFIFOOrder(t, oneIDs, expectedByShard, shards)
	if strings.Join(intsToStrings(oneIDs), ",") != strings.Join(intsToStrings(zeroIDs), ",") {
		t.Fatalf("workerCount=1 operation order = %v, want exact workerCount=0 parity %v", oneIDs, zeroIDs)
	}
}

func TestOperationTrackerDrainParallelStressWorkerCountTwo(t *testing.T) {
	runOperationTrackerDrainParallelStress(t, 2)
}

func TestOperationTrackerDrainParallelStressWorkerCountFour(t *testing.T) {
	result := runOperationTrackerDrainParallelStress(t, 4)
	wantRanges := [][2]int{{0, 2}, {2, 4}, {4, 6}, {6, 8}}
	if len(result.ranges) != len(wantRanges) {
		t.Fatalf("worker ranges = %v, want %v", result.ranges, wantRanges)
	}
	for i := range wantRanges {
		if result.ranges[i] != wantRanges[i] {
			t.Fatalf("worker range[%d] = %v, want %v", i, result.ranges[i], wantRanges[i])
		}
	}
}

func TestOperationTrackerDrainStressConcurrentNudgeAndDrainOnce(t *testing.T) {
	const (
		shards       = 8
		operations   = 256
		nudgeWorkers = 16
		drainWorkers = 8
		recordsPerOp = 1
	)
	manager := newTestSlotOperationTrackerManagerWithShards(t, shards, operations, recordsPerOp)
	emitter := &recordingEmitter{subscribed: true}
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetWorkerCount(4)
	worker.SetEmitter(emitter)

	for i := 0; i < operations; i++ {
		operationID := operationIdentityForShard(i%shards, i/shards+1, shards)
		handle, ok := manager.StartOperation(apiobs.InternalOperationIdentity(operationID), apiobs.ParentRef{}, 0)
		if !ok {
			t.Fatalf("StartOperation(%d) failed", operationID)
		}
		scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef(strconv.Itoa(operationID), ""))
		if !scope.OperationStartString("stress", apicommand.OperationID, strconv.Itoa(operationID)) {
			t.Fatalf("OperationStartString(%d) failed", operationID)
		}
		if !scope.Finish(commonobs.SlotTerminalFinished) {
			t.Fatalf("Finish(%d) failed", operationID)
		}
	}

	var wg sync.WaitGroup
	var drained atomic.Uint64
	for i := 0; i < drainWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drained.Add(uint64(worker.DrainOnce()))
		}()
	}
	for i := 0; i < nudgeWorkers; i++ {
		wg.Add(1)
		go func(workerIndex int) {
			defer wg.Done()
			for shard := 0; shard < shards; shard++ {
				worker.nudge((workerIndex + shard) % shards)
			}
		}(i)
	}
	wg.Wait()
	drained.Add(uint64(worker.DrainOnce()))

	if got := int(drained.Load()); got != operations {
		t.Fatalf("total drained = %d, want %d", got, operations)
	}
	ids := operationStartedIDs(t, emitter.snapshot())
	if len(ids) != operations {
		t.Fatalf("emitted operation.started events = %d, want %d", len(ids), operations)
	}
	assertNoDuplicateOperationIDs(t, ids, operations)
}

type drainParallelStressResult struct {
	ranges [][2]int
}

func runOperationTrackerDrainParallelStress(t *testing.T, workerCount int) drainParallelStressResult {
	t.Helper()
	const (
		shards              = 8
		producerGoroutines  = 50
		operationsPerWorker = 12
		totalOperations     = producerGoroutines * operationsPerWorker
	)
	manager := newTestSlotOperationTrackerManagerWithShards(t, shards, 256, 1)
	emitter := &recordingEmitter{subscribed: true}
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetWorkerCount(workerCount)
	ranges := workerRangeSnapshot(worker)
	worker.SetEmitter(emitter)
	worker.Start(context.Background())
	defer worker.Stop()

	expectedByShard := make([][]int, shards)
	shardLocks := make([]sync.Mutex, shards)
	var startFailures atomic.Uint64
	var recordFailures atomic.Uint64
	var finishFailures atomic.Uint64
	var wg sync.WaitGroup
	for producer := 0; producer < producerGoroutines; producer++ {
		producer := producer
		wg.Add(1)
		go func() {
			defer wg.Done()
			shard := producer % shards
			for seq := 0; seq < operationsPerWorker; seq++ {
				operationID := operationIdentityForShard(shard, producer*operationsPerWorker+seq+1, shards)
				shardLocks[shard].Lock()
				handle, ok := manager.StartOperation(apiobs.InternalOperationIdentity(operationID), apiobs.ParentRef{}, 0)
				if !ok {
					startFailures.Add(1)
					shardLocks[shard].Unlock()
					continue
				}
				scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef(strconv.Itoa(operationID), ""))
				if !scope.OperationStartString("stress", apicommand.OperationID, strconv.Itoa(operationID)) {
					recordFailures.Add(1)
				}
				if !scope.Finish(commonobs.SlotTerminalFinished) {
					finishFailures.Add(1)
				} else {
					expectedByShard[shard] = append(expectedByShard[shard], operationID)
				}
				shardLocks[shard].Unlock()
			}
		}()
	}
	wg.Wait()

	if got := startFailures.Load(); got != 0 {
		t.Fatalf("StartOperation failures = %d, want 0", got)
	}
	if got := recordFailures.Load(); got != 0 {
		t.Fatalf("OperationStartString failures = %d, want 0", got)
	}
	if got := finishFailures.Load(); got != 0 {
		t.Fatalf("Finish failures = %d, want 0", got)
	}
	waitForOperationStartedEvents(t, emitter, totalOperations)
	ids := operationStartedIDs(t, emitter.snapshot())
	if len(ids) != totalOperations {
		t.Fatalf("emitted operation.started events = %d, want %d", len(ids), totalOperations)
	}
	assertNoDuplicateOperationIDs(t, ids, totalOperations)
	assertShardFIFOOrder(t, ids, expectedByShard, shards)
	for shard := 0; shard < shards; shard++ {
		stats := manager.ShardStats(shard)
		if stats.CompletedSlots != 0 || stats.ActiveSlots != 0 {
			t.Fatalf("shard %d stats after stress = %+v, want no active/completed slots", shard, stats)
		}
	}
	return drainParallelStressResult{ranges: ranges}
}

func drainSerialParityOperationIDs(t *testing.T, workerCount, shardCount int) ([]int, [][]int) {
	t.Helper()
	manager := newTestSlotOperationTrackerManagerWithShards(t, shardCount, 16, 1)
	expectedByShard := make([][]int, shardCount)
	for shard := 0; shard < shardCount; shard++ {
		for seq := 1; seq <= 2; seq++ {
			operationID := operationIdentityForShard(shard, seq, shardCount)
			handle, ok := manager.StartOperation(apiobs.InternalOperationIdentity(operationID), apiobs.ParentRef{}, 0)
			if !ok {
				t.Fatalf("StartOperation(%d) failed", operationID)
			}
			scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef(strconv.Itoa(operationID), ""))
			if !scope.OperationStartString("serial", apicommand.OperationID, strconv.Itoa(operationID)) {
				t.Fatalf("OperationStartString(%d) failed", operationID)
			}
			if !scope.Finish(commonobs.SlotTerminalFinished) {
				t.Fatalf("Finish(%d) failed", operationID)
			}
			expectedByShard[shard] = append(expectedByShard[shard], operationID)
		}
	}
	emitter := &recordingEmitter{subscribed: true}
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetWorkerCount(workerCount)
	worker.SetEmitter(emitter)
	if drained := worker.DrainOnce(); drained != shardCount*2 {
		t.Fatalf("DrainOnce(workerCount=%d) = %d, want %d", workerCount, drained, shardCount*2)
	}
	return operationStartedIDs(t, emitter.snapshot()), expectedByShard
}

func newTestSlotOperationTrackerManagerWithShards(t *testing.T, shards, slots, recordsPerOperation int) *commonobs.SlotOperationTrackerManager {
	t.Helper()
	return commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{ShardCount: shards, MinSegmentsPerShard: 1, MaxSegmentsPerShard: 1, SegmentSize: slots, CompletedRingPerShard: slots})
}

func operationIdentityForShard(shard, seq, shardCount int) int {
	if shard == 0 {
		return seq * shardCount
	}
	return shard + seq*shardCount
}

func workerRangeSnapshot(worker *OperationTrackerDrainWorker) [][2]int {
	ranges := make([][2]int, len(worker.rangeWorkers))
	for i := range worker.rangeWorkers {
		ranges[i] = [2]int{worker.rangeWorkers[i].shardStart, worker.rangeWorkers[i].shardEnd}
	}
	return ranges
}

func waitForOperationStartedEvents(t *testing.T, emitter *recordingEmitter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := len(operationStartedIDs(t, emitter.snapshot())); got == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("operation.started events did not reach %d before deadline; got %d", want, len(operationStartedIDs(t, emitter.snapshot())))
}

func operationStartedIDs(t *testing.T, events []apievents.Event) []int {
	t.Helper()
	ids := make([]int, 0, len(events))
	for _, event := range events {
		if event.Proto == nil || event.Proto.Type != string(apievents.OperationStarted) {
			continue
		}
		id, err := strconv.Atoi(event.Proto.OperationId)
		if err != nil {
			t.Fatalf("operation_id %q is not an integer: %v", event.Proto.OperationId, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func assertNoDuplicateOperationIDs(t *testing.T, ids []int, wantTotal int) {
	t.Helper()
	seen := make(map[int]int, len(ids))
	for _, id := range ids {
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("operation_id %d drained %d times, want exactly once", id, seen[id])
		}
	}
	if len(seen) != wantTotal {
		t.Fatalf("unique drained operation IDs = %d, want %d", len(seen), wantTotal)
	}
}

func assertShardFIFOOrder(t *testing.T, drainedIDs []int, expectedByShard [][]int, shardCount int) {
	t.Helper()
	actualByShard := make([][]int, shardCount)
	for _, id := range drainedIDs {
		shard := id % shardCount
		actualByShard[shard] = append(actualByShard[shard], id)
	}
	for shard := 0; shard < shardCount; shard++ {
		actual := actualByShard[shard]
		expected := expectedByShard[shard]
		if strings.Join(intsToStrings(actual), ",") != strings.Join(intsToStrings(expected), ",") {
			t.Fatalf("shard %d drain order = %v, want FIFO completion order %v", shard, actual, expected)
		}
	}
}

func intsToStrings(values []int) []string {
	stringsOut := make([]string, len(values))
	for i, value := range values {
		stringsOut[i] = strconv.Itoa(value)
	}
	return stringsOut
}

func TestGapJanitorSampleEmitsPositiveDeltas(t *testing.T) {
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           2,
		CompletedRingPerShard: 1,
	})

	first, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("first operation should fit")
	}
	second, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("second operation should fit")
	}
	if _, ok := manager.StartOperation(3, apiobs.ParentRef{}, 0); ok {
		t.Fatal("third operation should skip with no free slots")
	}
	if !manager.RecordTelemetry(first, apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)) {
		t.Fatal("first telemetry record should fit")
	}
	// Under the arena model, records grow dynamically — both records fit.
	if !manager.RecordTelemetry(first, apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandFinish, 1)) {
		t.Fatal("second telemetry record should fit within arena growth")
	}
	if !manager.FinishOperation(first, commonobs.SlotTerminalFinished) {
		t.Fatal("first finish should enqueue")
	}
	if manager.FinishOperation(second, commonobs.SlotTerminalFailed) {
		t.Fatal("second finish should drop when completed ring is full")
	}
	if manager.RecordTelemetry(second, apiobs.NewTelemetryRecord(apiobs.TelemetryRecordLog, 2)) {
		t.Fatal("dropped completed handle should be invalid after reset")
	}

	janitor := newGapJanitor(time.Millisecond)
	if event := janitor.sample(manager, janitor.lastSampleTime.Add(time.Millisecond-time.Nanosecond)); event != nil {
		t.Fatalf("sample before interval emitted %q", event.Proto.Type)
	}
	event := janitor.sample(manager, janitor.lastSampleTime.Add(time.Millisecond))
	if event == nil {
		t.Fatal("sample after interval should emit replay.gap")
	}
	if event.Proto.Type != string(apievents.ReplayGap) {
		t.Fatalf("event type = %q, want %q", event.Proto.Type, apievents.ReplayGap)
	}
	gap := event.Proto.GetReplayGap()
	if gap == nil {
		t.Fatal("replay gap payload missing")
	}
	if gap.SkippedOperations != 1 || gap.DroppedRecords != 0 || gap.DroppedCompleted != 1 || gap.InvalidHandles != 1 || gap.WindowMs != 1 {
		t.Fatalf("gap payload = %+v, want all deltas=1 window_ms=1", gap)
	}
	if event := janitor.sample(manager, janitor.lastSampleTime.Add(time.Millisecond)); event != nil {
		t.Fatalf("zero-delta sample emitted %+v", event.Proto.GetReplayGap())
	}
}

func TestOperationTrackerDrainWorkerGapJanitorEmitsReplayGap(t *testing.T) {
	manager := newTestSlotOperationTrackerManager(t, 1, 1)
	active, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("active operation should fit")
	}
	if _, ok := manager.StartOperation(2, apiobs.ParentRef{}, 0); ok {
		t.Fatal("second operation should skip with no free slots")
	}

	emitter := &recordingEmitter{subscribed: true}
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(emitter)
	worker.SetGapInterval(time.Millisecond)
	worker.Start(context.Background())

	gap := waitForReplayGap(t, emitter)
	if gap.SkippedOperations != 1 || gap.WindowMs != 1 {
		t.Fatalf("gap payload = %+v, want skipped_operations=1 window_ms=1", gap)
	}
	worker.Stop()
	if !manager.FinishOperation(active, commonobs.SlotTerminalFinished) {
		t.Fatal("active operation should still be finishable after janitor stop")
	}
}

type recordingEmitter struct {
	mu         sync.Mutex
	subscribed bool
	events     []apievents.Event
}

func (r *recordingEmitter) Emit(event apievents.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingEmitter) snapshot() []apievents.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]apievents.Event(nil), r.events...)
}

func (r *recordingEmitter) HasSubscribers() bool { return r.subscribed }

func (r *recordingEmitter) HasSubscribersFor(...apievents.Type) bool { return r.subscribed }

func newTestSlotOperationTrackerManager(t *testing.T, slots, recordsPerOperation int) *commonobs.SlotOperationTrackerManager {
	t.Helper()
	return commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           slots,
		CompletedRingPerShard: slots,
	})
}

func waitForShardFreeSlots(t *testing.T, manager *commonobs.SlotOperationTrackerManager, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if stats := manager.ShardStats(0); stats.FreeSlots == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("free slots did not reach %d before deadline; stats=%+v", want, manager.ShardStats(0))
}

func waitForWorkerExit(t *testing.T, worker *OperationTrackerDrainWorker) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		worker.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after lifecycle context cancellation")
	}
}

func waitForReplayGap(t *testing.T, emitter *recordingEmitter) *gcpcv1.ReplayGapEventV1 {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, event := range emitter.snapshot() {
			if event.Proto != nil && event.Proto.Type == string(apievents.ReplayGap) {
				if gap := event.Proto.GetReplayGap(); gap != nil {
					return gap
				}
			}
		}
		runtime.Gosched()
	}
	t.Fatal("replay.gap event was not emitted before deadline")
	return nil
}

func decodeSingleLogEntry(t *testing.T, line string) map[string]any {
	t.Helper()
	entries := decodeLogEntries(t, line)
	if len(entries) != 1 {
		t.Fatalf("got %d log entries, want 1: %q", len(entries), line)
	}
	return entries[0]
}

func decodeLogEntries(t *testing.T, lines string) []map[string]any {
	t.Helper()
	lines = strings.TrimSpace(lines)
	if lines == "" {
		t.Fatal("no log entry materialized")
	}
	rawEntries := strings.Split(lines, "\n")
	entries := make([]map[string]any, 0, len(rawEntries))
	for _, raw := range rawEntries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("decode log entry %q: %v", raw, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func logContext(t *testing.T, entry map[string]any) map[string]any {
	t.Helper()
	ctx, ok := entry["_ctx"].(map[string]any)
	if !ok {
		t.Fatalf("log entry missing _ctx object: %+v", entry)
	}
	return ctx
}
