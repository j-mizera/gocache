//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	apilogger "gocache/api/logger"
	"gocache/testkit/integration"
)

func TestGobservability_TracesReachJaeger(t *testing.T) {
	jaeger := integration.StartJaeger(t)

	serviceName := "gocache-gobs-it"
	log := apilogger.New(os.Stderr, "test", "debug")

	ctx := context.Background()
	tracer, err := NewTracer(ctx, jaeger.OTLPEndpoint, serviceName, log)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}

	// Operation 1: successful command.
	tracer.StartOperation("op_1", "command", map[string]string{}, false, 0)
	tracer.CompleteOperation("op_1", "", map[string]string{})

	// Operation 2: failed snapshot.
	tracer.StartOperation("op_2", "snapshot", map[string]string{}, false, 0)
	tracer.CompleteOperation("op_2", "disk full", map[string]string{})

	if err := tracer.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	traces := integration.QueryTraces(t, jaeger.QueryEndpoint, serviceName, 10*time.Second)

	cmdSpans := integration.SpansByName(traces, "gocache.command")
	if len(cmdSpans) != 1 {
		t.Fatalf("expected 1 gocache.command span, got %d", len(cmdSpans))
	}
	if cmdSpans[0].Tags["otel.status_code"] != "OK" {
		t.Errorf("command span status = %q, want OK", cmdSpans[0].Tags["otel.status_code"])
	}

	snapSpans := integration.SpansByName(traces, "gocache.snapshot")
	if len(snapSpans) != 1 {
		t.Fatalf("expected 1 gocache.snapshot span, got %d", len(snapSpans))
	}
	if snapSpans[0].Tags["otel.status_code"] != "ERROR" {
		t.Errorf("snapshot span status = %q, want ERROR", snapSpans[0].Tags["otel.status_code"])
	}
	if snapSpans[0].Tags["otel.status_description"] != "disk full" {
		t.Errorf("snapshot span description = %q, want 'disk full'", snapSpans[0].Tags["otel.status_description"])
	}

	// Both spans should have the component attribute.
	for _, sp := range append(cmdSpans, snapSpans...) {
		if sp.Tags[attrComponent] != componentValue {
			t.Errorf("span %q component = %q, want %q", sp.OperationName, sp.Tags[attrComponent], componentValue)
		}
	}
}
