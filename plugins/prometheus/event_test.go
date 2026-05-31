package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"gocache/api/scope"
	"gocache/sdk/pluginsdk"
)

func TestPrometheusRuntimeTelemetryUsesMetricsQueryOnly(t *testing.T) {
	plugin := &prometheusPlugin{}

	if _, ok := any(plugin).(pluginsdk.HookPlugin); ok {
		t.Fatal("prometheus must not register command hooks for runtime telemetry")
	}
	if _, ok := any(plugin).(pluginsdk.OperationHookPlugin); ok {
		t.Fatal("prometheus must not register operation hooks for runtime telemetry")
	}
	if _, ok := any(plugin).(pluginsdk.EventPlugin); ok {
		t.Fatal("prometheus must not subscribe to per-command events for metrics")
	}

	scopes := map[string]bool{}
	for _, s := range plugin.Scopes() {
		scopes[s] = true
	}
	if scopes[string(scope.ScopeEvents)] {
		t.Fatal("prometheus must not request events scope for metrics-only telemetry")
	}
	for _, want := range []string{
		string(scope.ScopeServerQueryHealth),
		string(scope.ScopeServerQueryPlugins),
		string(scope.ScopeServerQueryMetricsCommands),
	} {
		if !scopes[want] {
			t.Fatalf("prometheus scopes missing %q: %v", want, plugin.Scopes())
		}
	}
}

func TestPrometheusReplacesCommandMetricsFromQuerySnapshot(t *testing.T) {
	collector := NewCollector()
	data := map[string]string{
		"buckets.count":    "9",
		"commands.count":   "1",
		"command.0.name":   "SET",
		"command.0.total":  "2",
		"command.0.errors": "1",
		"command.0.sum_ns": "3000000",
	}
	for i := 0; i < len(defaultBuckets)+1; i++ {
		data["command.0.bucket."+strconv.Itoa(i)] = "0"
	}
	data["command.0.bucket.1"] = "1"
	data["command.0.bucket.2"] = "1"

	if err := collector.ReplaceFromQuery(data); err != nil {
		t.Fatalf("ReplaceFromQuery error: %v", err)
	}

	collector.mu.Lock()
	stats := collector.stats["SET"]
	collector.mu.Unlock()
	if stats == nil {
		t.Fatal("expected SET metrics")
	}
	if stats.total != 2 {
		t.Fatalf("total = %d, want 2", stats.total)
	}
	if stats.errors != 1 {
		t.Fatalf("errors = %d, want 1", stats.errors)
	}
	if stats.sum != 0.003 {
		t.Fatalf("sum = %g, want 0.003", stats.sum)
	}
}

func TestPrometheusMetricsHandlerRendersCollectorWithoutSession(t *testing.T) {
	plugin := &prometheusPlugin{collector: NewCollector()}
	plugin.collector.Record("PING", 42_000, false)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	metricsHandler(plugin, pluginName, "test").ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("gocache_commands_total{command=\"PING\"} 1")) {
		t.Fatalf("metrics output missing PING counter:\n%s", rec.Body.String())
	}
}
