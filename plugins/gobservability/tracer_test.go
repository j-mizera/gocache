package main

import (
	"context"
	"os"
	"testing"

	apilogger "gocache/api/logger"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newTestTracer() (*Tracer, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	log := apilogger.New(os.Stderr, "test", "debug")
	return &Tracer{
		provider: tp,
		tracer:   tp.Tracer("test"),
		log:      log,
		inflight: make(map[string]inflightSpan),
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
		{"bad hex", "00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01", false},
		{"too few parts", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", false},
		{"empty", "", false},
		{"all zeros trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, valid := parseTraceparent(tt.input)
			if valid != tt.want {
				t.Errorf("parseTraceparent(%q) valid=%v, want %v", tt.input, valid, tt.want)
			}
		})
	}
}

func TestStartOperation_WithTraceparent(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	ctx := map[string]string{
		"shared.traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}

	tp := tracer.StartOperation("cmd_1", "command", ctx)
	if tp == "" {
		t.Fatal("expected non-empty traceparent")
	}

	tracer.CompleteOperation("cmd_1", "completed", "", ctx)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	span := spans[0]
	if span.Name != "gocache.command" {
		t.Errorf("expected span name gocache.command, got %q", span.Name)
	}
	if span.SpanKind != trace.SpanKindServer {
		t.Errorf("expected SpanKindServer, got %v", span.SpanKind)
	}
	// Should be a child of the provided traceparent.
	if span.Parent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("parent trace id: got %s", span.Parent.TraceID().String())
	}
}

func TestStartOperation_WithoutTraceparent(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	tp := tracer.StartOperation("cleanup_1", "cleanup", map[string]string{})
	if tp == "" {
		t.Fatal("expected generated traceparent")
	}

	tracer.CompleteOperation("cleanup_1", "completed", "", map[string]string{})

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name != "gocache.cleanup" {
		t.Errorf("expected gocache.cleanup, got %q", spans[0].Name)
	}
	// Root span — no parent.
	if spans[0].Parent.IsValid() {
		t.Error("expected no parent (root span)")
	}
}

func TestStartOperation_REXFallback(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	// Only shared.rex.traceparent set (no shared.traceparent).
	ctx := map[string]string{
		"shared.rex.traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}

	tracer.StartOperation("cmd_2", "command", ctx)
	tracer.CompleteOperation("cmd_2", "completed", "", ctx)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	// Should use the REX traceparent as parent.
	if spans[0].Parent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("expected REX traceparent as parent, got %s", spans[0].Parent.TraceID().String())
	}
}

func TestCompleteOperation_Failed(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	tracer.StartOperation("snap_1", "snapshot", map[string]string{})
	tracer.CompleteOperation("snap_1", "failed", "disk full", map[string]string{})

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("expected Error status, got %v", spans[0].Status.Code)
	}
	if spans[0].Status.Description != "disk full" {
		t.Errorf("expected 'disk full', got %q", spans[0].Status.Description)
	}
}

func TestCompleteOperation_SecretsRedacted(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	tracer.StartOperation("cmd_3", "command", map[string]string{})

	ctx := map[string]string{
		"shared.username":   "john",
		"shared.secret.jwt": "eyJ...",
		"_command":          "SET",
		"_secret.session":   "tok",
	}
	tracer.CompleteOperation("cmd_3", "completed", "", ctx)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	// Check attributes — secrets should be redacted.
	attrs := make(map[string]string)
	for _, a := range spans[0].Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}

	if attrs["shared.username"] != "john" {
		t.Error("expected shared.username in attributes")
	}
	if attrs["_command"] != "SET" {
		t.Error("expected _command in attributes")
	}
	if _, ok := attrs["shared.secret.jwt"]; ok {
		t.Error("shared.secret.jwt should be redacted")
	}
	if _, ok := attrs["_secret.session"]; ok {
		t.Error("_secret.session should be redacted")
	}
}

func TestCompleteOperation_Unknown(t *testing.T) {
	tracer, _ := newTestTracer()
	defer tracer.Shutdown(context.Background())

	// Should not panic.
	tracer.CompleteOperation("nonexistent", "completed", "", nil)
}

func TestNilTracer(t *testing.T) {
	var tracer *Tracer
	// Should not panic.
	tp := tracer.StartOperation("cmd_1", "command", nil)
	if tp != "" {
		t.Error("expected empty traceparent from nil tracer")
	}
	tracer.CompleteOperation("cmd_1", "completed", "", nil)
}

func TestRecordLog_AttachesToSpan(t *testing.T) {
	tracer, exporter := newTestTracer()
	defer tracer.Shutdown(context.Background())

	tracer.StartOperation("cmd_10", "command", map[string]string{})

	tracer.RecordLog("cmd_10", "info", "cache hit", map[string]string{
		"_source":           "server",
		"shared.username":   "john",
		"shared.secret.jwt": "eyJ...",
		"key":               "user:123",
	})

	tracer.CompleteOperation("cmd_10", "completed", "", map[string]string{})

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	events := spans[0].Events
	if len(events) != 1 {
		t.Fatalf("expected 1 event on span, got %d", len(events))
	}

	evt := events[0]
	if evt.Name != "cache hit" {
		t.Errorf("expected event name 'cache hit', got %q", evt.Name)
	}

	// Check attributes — secrets should be redacted.
	attrs := make(map[string]string)
	for _, a := range evt.Attributes {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["log.level"] != "info" {
		t.Error("expected log.level=info")
	}
	if attrs["key"] != "user:123" {
		t.Error("expected key=user:123")
	}
	if attrs["shared.username"] != "john" {
		t.Error("expected shared.username=john")
	}
	if _, ok := attrs["shared.secret.jwt"]; ok {
		t.Error("shared.secret.jwt should be redacted")
	}
}

func TestRecordLog_NoInflightSpan(t *testing.T) {
	tracer, _ := newTestTracer()
	defer tracer.Shutdown(context.Background())

	// Should not panic — operation already completed or doesn't exist.
	tracer.RecordLog("nonexistent", "warn", "orphaned log", nil)
}

func TestRecordLog_NilTracer(t *testing.T) {
	var tracer *Tracer
	tracer.RecordLog("cmd_1", "info", "test", nil)
}

func TestGenerateTraceparent(t *testing.T) {
	tp := GenerateTraceparent()
	if len(tp) == 0 {
		t.Fatal("expected non-empty traceparent")
	}
	// Should be parseable.
	_, valid := parseTraceparent(tp)
	if !valid {
		t.Errorf("generated traceparent %q is not valid", tp)
	}
}
