package benchstats

import "testing"

func TestSnapshotDisabledStillIncludesRuntimeMetrics(t *testing.T) {
	Reset()
	SetEnabled(false)
	t.Cleanup(func() {
		Reset()
		SetEnabled(false)
	})

	RecordPipelineEvaluation()
	data := Snapshot(false)
	if data["enabled"] != "false" {
		t.Fatalf("enabled=%q, want false", data["enabled"])
	}
	if data["pipeline.evaluations"] != "0" {
		t.Fatalf("pipeline.evaluations=%q, want 0", data["pipeline.evaluations"])
	}
	if data["runtime.sched.goroutines.goroutines"] == "" {
		t.Fatal("expected runtime goroutine metric")
	}
}

func TestSnapshotReset(t *testing.T) {
	Reset()
	SetEnabled(true)
	t.Cleanup(func() {
		Reset()
		SetEnabled(false)
	})

	RecordPipelineEvaluation()
	RecordPipelineOperationStarted()
	RecordPipelineOperationCompleted()
	RecordManagerEventReceived()
	RecordManagerProjectionBuild()
	RecordManagerEventEnqueue()

	data := Snapshot(true)
	checks := map[string]string{
		"enabled":                            "true",
		"pipeline.evaluations":               "1",
		"pipeline.event.operation_started":   "1",
		"pipeline.event.operation_completed": "1",
		"manager.event_received":             "1",
		"manager.event_enqueue_attempts":     "1",
		"manager.projection_builds":          "1",
	}
	for key, want := range checks {
		if got := data[key]; got != want {
			t.Fatalf("data[%q]=%q, want %q", key, got, want)
		}
	}
	if got := Snapshot(false)["pipeline.evaluations"]; got != "0" {
		t.Fatalf("post-reset pipeline.evaluations=%q, want 0", got)
	}
}
