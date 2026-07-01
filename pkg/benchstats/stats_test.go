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
	checks := map[string]string{
		"enabled":                    "false",
		"pipeline.evaluations":       "0",
		"pipeline.command_unknown":   "0",
		"pipeline.command_arg_error": "0",
		"pipeline.command_queued":    "0",
		"pipeline.plugin_routed":     "0",
	}
	for key, want := range checks {
		if got := data[key]; got != want {
			t.Fatalf("data[%q]=%q, want %q", key, got, want)
		}
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
	RecordPipelineCommandUnknown()
	RecordPipelineCommandArgError()
	RecordPipelineCommandQueued()
	RecordPipelinePluginRouted()
	RecordManagerEventReceived()
	RecordManagerEventDropped()
	RecordManagerProjectionBuild()
	RecordManagerEventEnqueue()

	data := Snapshot(true)
	checks := map[string]string{
		"enabled":                        "true",
		"pipeline.evaluations":           "1",
		"pipeline.command_unknown":       "1",
		"pipeline.command_arg_error":     "1",
		"pipeline.command_queued":        "1",
		"pipeline.plugin_routed":         "1",
		"manager.event_received":         "1",
		"manager.event_dropped":          "1",
		"manager.event_enqueue_attempts": "1",
		"manager.projection_builds":      "1",
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
