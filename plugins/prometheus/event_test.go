package main

import (
	"context"
	"testing"

	apiEvents "gocache/api/events"
	"gocache/sdk/pluginsdk"
)

func TestPrometheusRuntimeTelemetryUsesEventsOnly(t *testing.T) {
	plugin := &prometheusPlugin{}

	if _, ok := any(plugin).(pluginsdk.HookPlugin); ok {
		t.Fatal("prometheus must not register command hooks for runtime telemetry")
	}
	if _, ok := any(plugin).(pluginsdk.OperationHookPlugin); ok {
		t.Fatal("prometheus must not register operation hooks for runtime telemetry")
	}

	got := plugin.EventTypes()
	if len(got) != 1 || got[0] != string(apiEvents.CommandPost) {
		t.Fatalf("event subscriptions = %v, want only %q", got, apiEvents.CommandPost)
	}
}

func TestPrometheusRecordsCommandMetricsFromCommandPostEvent(t *testing.T) {
	plugin := &prometheusPlugin{collector: NewCollector()}
	evt := apiEvents.NewCommandPost("SET", []string{"k", "v"}, 42_000, "OK", "", nil)

	plugin.HandleEvent(context.Background(), evt.Proto)

	plugin.collector.mu.Lock()
	defer plugin.collector.mu.Unlock()
	stats := plugin.collector.stats["SET"]
	if stats == nil {
		t.Fatal("expected SET metrics")
	}
	if stats.total != 1 {
		t.Fatalf("total = %d, want 1", stats.total)
	}
	if stats.errors != 0 {
		t.Fatalf("errors = %d, want 0", stats.errors)
	}
}

func TestPrometheusIgnoresNonMetricEvents(t *testing.T) {
	plugin := &prometheusPlugin{collector: NewCollector()}

	plugin.HandleEvent(context.Background(), apiEvents.NewOperationStart("cmd_1", "command", "", nil).Proto)
	plugin.HandleEvent(context.Background(), apiEvents.NewOperationComplete("cmd_1", "command", 10_000, "failed", "disk full", nil).Proto)
	plugin.HandleEvent(context.Background(), apiEvents.NewLogEntry("info", "hello", "test", nil).WithOperationID("cmd_1").Proto)

	plugin.collector.mu.Lock()
	defer plugin.collector.mu.Unlock()
	if len(plugin.collector.stats) != 0 {
		t.Fatalf("unexpected metrics from non-command.post events: %#v", plugin.collector.stats)
	}
}
