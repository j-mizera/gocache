package main

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newTestTracer creates a Tracer backed by an in-memory exporter.
func newTestTracer() (*Tracer, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	return &Tracer{
		provider: tp,
		tracer:   tp.Tracer("test"),
	}, exporter
}

func TestParseTraceparent_Valid(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid sampled", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", true},
		{"valid not sampled", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", true},
		{"wrong version", "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", false},
		{"short trace id", "00-4bf92f-00f067aa0ba902b7-01", false},
		{"short span id", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067-01", false},
		{"bad hex in trace id", "00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01", false},
		{"too few parts", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", false},
		{"empty", "", false},
		{"all zeros trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc, valid := parseTraceparent(tt.input)
			if valid != tt.want {
				t.Errorf("parseTraceparent(%q) valid=%v, want %v", tt.input, valid, tt.want)
			}
			if valid {
				if !sc.HasTraceID() {
					t.Error("expected valid trace ID")
				}
				if !sc.HasSpanID() {
					t.Error("expected valid span ID")
				}
				if !sc.IsRemote() {
					t.Error("expected remote span context")
				}
			}
		})
	}
}

func TestParseTraceparent_Fields(t *testing.T) {
	sc, valid := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !valid {
		t.Fatal("expected valid")
	}

	if sc.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace id: got %s", sc.TraceID().String())
	}
	if sc.SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("span id: got %s", sc.SpanID().String())
	}
	if !sc.IsSampled() {
		t.Error("expected sampled flag set")
	}
}

func TestRecordCommand_WithTraceparent(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	now := time.Now()
	startNs := uint64(now.UnixNano())
	elapsedNs := uint64(5 * time.Millisecond)

	metadata := map[string]string{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}

	tracer.RecordCommand("SET", []string{"key", "value"}, elapsedNs, startNs, false, "", metadata)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name != "gocache SET" {
		t.Errorf("expected name 'gocache SET', got %q", span.Name)
	}
	if span.SpanKind != trace.SpanKindServer {
		t.Errorf("expected SpanKindServer, got %v", span.SpanKind)
	}
	if span.Status.Code != codes.Ok {
		t.Errorf("expected status Ok, got %v", span.Status.Code)
	}

	// Verify parent trace ID matches.
	if span.Parent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("parent trace id: got %s", span.Parent.TraceID().String())
	}

	// Verify attributes.
	hasAttr := func(key string) bool {
		for _, a := range span.Attributes {
			if string(a.Key) == key {
				return true
			}
		}
		return false
	}
	if !hasAttr("db.system") {
		t.Error("missing db.system attribute")
	}
	if !hasAttr("db.operation.name") {
		t.Error("missing db.operation.name attribute")
	}

	// Check arg count attribute.
	for _, a := range span.Attributes {
		if a.Key == attribute.Key("gocache.arg_count") {
			if a.Value.AsInt64() != 2 {
				t.Errorf("expected arg_count=2, got %d", a.Value.AsInt64())
			}
		}
	}
}

func TestRecordCommand_WithoutTraceparent(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	now := time.Now()
	tracer.RecordCommand("GET", []string{"key"}, uint64(time.Millisecond), uint64(now.UnixNano()), false, "", nil)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name != "gocache GET" {
		t.Errorf("expected 'gocache GET', got %q", spans[0].Name)
	}
	// No parent — root span.
	if spans[0].Parent.IsValid() {
		t.Error("expected no parent (root span)")
	}
}

func TestRecordCommand_Error(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	now := time.Now()
	tracer.RecordCommand("SET", []string{"key", "value"}, uint64(time.Millisecond), uint64(now.UnixNano()), true, "ERR out of memory", nil)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("expected Error status, got %v", spans[0].Status.Code)
	}
	if spans[0].Status.Description != "ERR out of memory" {
		t.Errorf("expected error description, got %q", spans[0].Status.Description)
	}
}

func TestRecordCommand_NilTracer(t *testing.T) {
	// Should not panic.
	var tracer *Tracer
	tracer.RecordCommand("SET", nil, 0, 0, false, "", nil)
}

func TestRecordCommand_Timing(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	startNs := uint64(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	elapsedNs := uint64(10 * time.Millisecond)

	tracer.RecordCommand("PING", nil, elapsedNs, startNs, false, "", nil)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	expectedStart := time.Unix(0, int64(startNs))
	expectedEnd := expectedStart.Add(time.Duration(elapsedNs))

	if !span.StartTime.Equal(expectedStart) {
		t.Errorf("start time: got %v, want %v", span.StartTime, expectedStart)
	}
	if !span.EndTime.Equal(expectedEnd) {
		t.Errorf("end time: got %v, want %v", span.EndTime, expectedEnd)
	}
}
