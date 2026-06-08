package server

import (
	"testing"
	"time"

	apicommand "gocache/api/command"
	apiobs "gocache/api/observability"
	commonobs "gocache/commons/observability"
)

func TestOperationTrackerDrainWorkerEmitsImmutableCommandPayloads(t *testing.T) {
	manager := newTestSlotOperationTrackerManager(t, 1, 2)
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate a slot")
	}
	scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef("cmd-test", ""))
	if !scope.CommandStartString("SET", apicommand.OperationID, "cmd-test", "_args_count", "2", "_arg.0", "key", "_arg.1", "value", "_metadata.shared.traceparent", "00-original") {
		t.Fatal("command start record should fit")
	}
	if !scope.CommandFinishString("SET", 123, apicommand.OperationID, "cmd-test", "_args_count", "1", "_arg.0", "key", "_metadata.shared.traceparent", "00-finished") {
		t.Fatal("command finish record should fit")
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

	if len(emitter.events) != 2 {
		t.Fatalf("emitted events = %d, want 2", len(emitter.events))
	}
	started := emitter.events[0].Proto.GetCommandPre()
	finished := emitter.events[1].Proto.GetCommandPost()
	if started == nil || finished == nil {
		t.Fatalf("missing command event payloads: start=%v finish=%v", started, finished)
	}
	if got := started.Args[0]; got != "key" {
		t.Fatalf("started args[0] = %q, want key", got)
	}
	if got := started.Args[1]; got != "value" {
		t.Fatalf("started args[1] = %q, want value", got)
	}
	if got := started.Metadata["shared.traceparent"]; got != "00-original" {
		t.Fatalf("started metadata = %q, want 00-original", got)
	}
	if got := finished.Args[0]; got != "key" {
		t.Fatalf("finished args[0] = %q, want key", got)
	}
	if got := finished.Metadata["shared.traceparent"]; got != "00-finished" {
		t.Fatalf("finished metadata = %q, want 00-finished", got)
	}
}

func TestOperationTrackerDrainWorkerEmitsImmutableOperationContext(t *testing.T) {
	manager := newTestSlotOperationTrackerManager(t, 1, 3)
	handle, _, ok := manager.StartOperationWithConnectionContext(1, apiobs.ParentRef{}, apiobs.ConnectionIdentity(1), map[string]string{"tenant": "acme"})
	if !ok {
		t.Fatal("StartOperationWithConnectionContext should allocate a slot")
	}
	scope := commonobs.NewOperationScope(manager, handle, 1, apiobs.NewOperationRef("cmd-test", ""))
	if !scope.OperationStartString("command", apicommand.OperationID, "cmd-test") {
		t.Fatal("operation start record should fit")
	}
	if !scope.ContextUpdate([]byte("tenant"), []byte("globex")) {
		t.Fatal("context update should fit")
	}
	if !scope.OperationFinishString("command", 123, apicommand.OperationID, "cmd-test", "_status", "completed") {
		t.Fatal("operation finish record should fit")
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

	if len(emitter.events) != 2 {
		t.Fatalf("emitted events = %d, want 2", len(emitter.events))
	}
	started := emitter.events[0].Proto.GetOperationStart()
	finished := emitter.events[1].Proto.GetOperationComplete()
	if started == nil || finished == nil {
		t.Fatalf("missing operation event payloads: start=%v finish=%v", started, finished)
	}
	if got := started.Context["tenant"]; got != "acme" {
		t.Fatalf("started context tenant = %q, want acme", got)
	}
	if got := finished.Context["tenant"]; got != "globex" {
		t.Fatalf("finished context tenant = %q, want globex", got)
	}
}
