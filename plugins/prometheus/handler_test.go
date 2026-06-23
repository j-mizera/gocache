package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeServerQuerier struct {
	topic    string
	snapshot map[string]string
	queryErr error
}

func (q *fakeServerQuerier) QueryServer(_ context.Context, topic string, _ map[string]string) (map[string]string, error) {
	q.topic = topic
	return q.snapshot, q.queryErr
}

func TestTelemetryHandlerReturnsUnavailableWithoutSession(t *testing.T) {
	plugin := &prometheusPlugin{}
	req := httptest.NewRequest(http.MethodGet, "/telemetry", nil)
	rec := httptest.NewRecorder()

	telemetryHandler(plugin).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["status"] != "initializing" {
		t.Fatalf("status body=%q, want initializing", response["status"])
	}
	if response["hint"] == "" {
		t.Fatal("expected missing-session hint")
	}
}

func TestTelemetryHandlerReturnsQuerySnapshot(t *testing.T) {
	querier := &fakeServerQuerier{snapshot: map[string]string{
		"telemetry.skipped_operations": "2",
		"telemetry.shard.0.active":     "1",
	}}
	plugin := &prometheusPlugin{session: querier}
	req := httptest.NewRequest(http.MethodGet, "/telemetry", nil)
	rec := httptest.NewRecorder()

	telemetryHandler(plugin).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if querier.topic != "metrics.telemetry" {
		t.Fatalf("topic=%q, want metrics.telemetry", querier.topic)
	}
	var response map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["telemetry.skipped_operations"] != "2" {
		t.Fatalf("telemetry.skipped_operations=%q, want 2", response["telemetry.skipped_operations"])
	}
}

func TestTelemetryHandlerReturnsQueryErrorWithScopeHint(t *testing.T) {
	querier := &fakeServerQuerier{queryErr: errors.New("permission denied")}
	plugin := &prometheusPlugin{session: querier}
	req := httptest.NewRequest(http.MethodGet, "/telemetry", nil)
	rec := httptest.NewRecorder()

	telemetryHandler(plugin).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["error"] != "permission denied" {
		t.Fatalf("error=%q, want permission denied", response["error"])
	}
	if response["hint"] == "" {
		t.Fatal("expected query error hint")
	}
}
