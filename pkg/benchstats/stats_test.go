package benchstats

import (
	"testing"
	"time"
)

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

	start := time.Now().Add(-time.Nanosecond)
	RecordPipelineEvaluation()
	RecordPipelineFullPath()
	RecordPipelineContextSnapshot(start)
	RecordPipelineOperationStartedBuilt(start)
	RecordEventBusInterestCheck(true)
	RecordEventBusEmit(3, start)
	RecordEventBusNoSubscriberEmit(start)
	RecordEventBusDelivery(start)
	RecordManagerBridgeHandler(start)
	RecordManagerEventEnqueue(start)
	RecordManagerProjection(start)

	data := Snapshot(true)
	checks := map[string]string{
		"enabled":                          "true",
		"pipeline.evaluations":             "1",
		"pipeline.path.full":               "1",
		"pipeline.context_snapshots":       "1",
		"pipeline.event.operation_started": "1",
		"event_bus.interest_checks":        "1",
		"event_bus.interest_hits":          "1",
		"event_bus.emits":                  "1",
		"event_bus.emits_no_subscribers":   "1",
		"event_bus.fanout_targets_total":   "3",
		"event_bus.fanout_targets_max":     "3",
		"event_bus.deliveries":             "1",
		"manager.bridge_handler_runs":      "1",
		"manager.event_enqueue_attempts":   "1",
		"manager.projection_builds":        "1",
	}
	for key, want := range checks {
		if got := data[key]; got != want {
			t.Fatalf("data[%q]=%q, want %q", key, got, want)
		}
	}
	for _, key := range []string{
		"pipeline.context_snapshot_total_ns",
		"pipeline.event.operation_started_build_ns",
		"event_bus.emit_latency_total_ns",
		"event_bus.emit_no_subscriber_latency_total_ns",
		"event_bus.delivery_latency_total_ns",
		"manager.bridge_handler_latency_total_ns",
		"manager.event_enqueue_latency_total_ns",
		"manager.projection_latency_total_ns",
	} {
		if got := data[key]; got == "" || got == "0" {
			t.Fatalf("data[%q]=%q, want non-zero duration", key, got)
		}
	}

	if got := Snapshot(false)["pipeline.evaluations"]; got != "0" {
		t.Fatalf("post-reset pipeline.evaluations=%q, want 0", got)
	}
}
