package observability

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	apiobs "gocache/api/observability"
	"gocache/commons/logger"
)

func TestStartupLogMaterializerRecordsAndMaterializesAcceptedLog(t *testing.T) {
	var output bytes.Buffer
	logger.InitWithWriter(&output, "debug")
	t.Cleanup(func() { logger.Init("info") })

	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   2,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(33, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate startup slot")
	}
	scope := NewOperationScope(manager, handle, 33, apiobs.NewOperationRef("startup-public", ""))

	record := apiobs.NewLogRecordString(scope.Operation(), apiobs.TelemetryLogLevelInfo, "startup ready")
	record.AddFieldString("addr", "127.0.0.1:6379")
	if !(StartupLogMaterializer{}).LogRecord(scope, record) {
		t.Fatal("startup log should be accepted and materialized")
	}
	if !scope.Finish(SlotTerminalFinished) {
		t.Fatal("startup scope should finish")
	}

	line := output.String()
	for _, want := range []string{`"level":"info"`, `"_operation_id":"startup-public"`, `"addr":"127.0.0.1:6379"`, `"message":"startup ready"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("materialized log %q missing %s", line, want)
		}
	}

	var completed CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation CompletedOperation) {
		completed = cloneCompletedOperation(operation)
	}); drained != 1 {
		t.Fatalf("drained %d operations, want 1", drained)
	}
	if completed.Status != SlotTerminalFinished {
		t.Fatalf("completed status = %v, want finished", completed.Status)
	}
	if len(completed.Records) != 1 {
		t.Fatalf("record count = %d, want 1", len(completed.Records))
	}
	accepted := completed.Records[0]
	if accepted.Flags&apiobs.TelemetryRecordFlagLocalLogMaterialized == 0 {
		t.Fatal("accepted startup log should be marked local-materialized")
	}
	key, value, ok := accepted.FieldBytes(0)
	if !ok || string(key) != "addr" || string(value) != "127.0.0.1:6379" {
		t.Fatalf("field[0] = %q/%q/%v, want addr/127.0.0.1:6379/true", key, value, ok)
	}
}

func TestStartupLogMaterializerDoesNotMaterializeRejectedLog(t *testing.T) {
	var output bytes.Buffer
	logger.InitWithWriter(&output, "debug")
	t.Cleanup(func() { logger.Init("info") })

	if (StartupLogMaterializer{}).LogString(OperationScope{}, apiobs.TelemetryLogLevelFatal, "ignored") {
		t.Fatal("zero scope should reject startup log")
	}
	if output.Len() != 0 {
		t.Fatalf("rejected startup log wrote %q", output.String())
	}
}

func TestStartupLogMaterializerFatalAndPanicLevelsDoNotExitOrPanic(t *testing.T) {
	for _, level := range []string{"fatal", "panic"} {
		t.Run(level, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestStartupLogMaterializerLevelHelper")
			cmd.Env = append(os.Environ(), "GOCACHE_STARTUP_MATERIALIZER_LEVEL="+level)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helper for %s level failed: %v\n%s", level, err, out)
			}
		})
	}
}

func TestStartupLogMaterializerLevelHelper(t *testing.T) {
	levelName := os.Getenv("GOCACHE_STARTUP_MATERIALIZER_LEVEL")
	if levelName == "" {
		return
	}

	level := apiobs.TelemetryLogLevelFatal
	if levelName == "panic" {
		level = apiobs.TelemetryLogLevelPanic
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			os.Exit(2)
		}
	}()

	var output bytes.Buffer
	logger.InitWithWriter(&output, "debug")
	manager := NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   1,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(34, apiobs.ParentRef{}, 0)
	if !ok {
		os.Exit(3)
	}
	scope := NewOperationScope(manager, handle, 34, apiobs.NewOperationRef("startup-level", ""))
	if !(StartupLogMaterializer{}).LogString(scope, level, "level check") {
		os.Exit(4)
	}
	if !strings.Contains(output.String(), `"message":"level check"`) {
		os.Exit(5)
	}
	os.Exit(0)
}
