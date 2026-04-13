package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracer wraps an OTEL TracerProvider and creates spans from command hook data.
type Tracer struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

// NewTracer creates a Tracer with an OTLP HTTP exporter.
func NewTracer(endpoint, serviceName string) (*Tracer, error) {
	ctx := context.Background()

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
	}
	// If endpoint doesn't start with https, use insecure.
	if !strings.HasPrefix(endpoint, "https") {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			attribute.String("gocache.component", "gobservability"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	return &Tracer{
		provider: tp,
		tracer:   tp.Tracer("gocache.gobservability"),
	}, nil
}

// RecordCommand creates a span from a post-hook invocation.
// If metadata contains a W3C traceparent, the span is created as a child.
// Timing is reconstructed from _start_ns and _elapsed_ns.
func (t *Tracer) RecordCommand(command string, args []string, elapsedNs uint64, startNs uint64, isError bool, resultError string, metadata map[string]string) {
	if t == nil {
		return
	}

	ctx := context.Background()

	// Parse traceparent from metadata if present.
	if tp, ok := metadata["traceparent"]; ok {
		if sc, valid := parseTraceparent(tp); valid {
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		}
	}

	// Reconstruct timing.
	startTime := time.Unix(0, int64(startNs))
	endTime := startTime.Add(time.Duration(elapsedNs))

	_, span := t.tracer.Start(ctx, "gocache "+command,
		trace.WithTimestamp(startTime),
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			semconv.DBSystemKey.String("gocache"),
			semconv.DBOperationName(command),
			attribute.Int("gocache.arg_count", len(args)),
		),
	)

	if isError {
		span.SetStatus(codes.Error, resultError)
	} else {
		span.SetStatus(codes.Ok, "")
	}

	span.End(trace.WithTimestamp(endTime))
}

// Shutdown flushes pending spans and shuts down the exporter.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t == nil || t.provider == nil {
		return nil
	}
	return t.provider.Shutdown(ctx)
}

// parseTraceparent parses a W3C traceparent header.
// Format: version-traceid-spanid-flags (e.g. "00-abc123...-def456...-01")
func parseTraceparent(tp string) (trace.SpanContext, bool) {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		return trace.SpanContext{}, false
	}

	// version must be "00"
	if parts[0] != "00" {
		return trace.SpanContext{}, false
	}

	traceIDHex := parts[1]
	spanIDHex := parts[2]
	flagsHex := parts[3]

	// Trace ID must be 32 hex chars (16 bytes).
	if len(traceIDHex) != 32 {
		return trace.SpanContext{}, false
	}
	traceIDBytes, err := hex.DecodeString(traceIDHex)
	if err != nil {
		return trace.SpanContext{}, false
	}
	var traceID trace.TraceID
	copy(traceID[:], traceIDBytes)

	// Span ID must be 16 hex chars (8 bytes).
	if len(spanIDHex) != 16 {
		return trace.SpanContext{}, false
	}
	spanIDBytes, err := hex.DecodeString(spanIDHex)
	if err != nil {
		return trace.SpanContext{}, false
	}
	var spanID trace.SpanID
	copy(spanID[:], spanIDBytes)

	// Flags: 1 byte hex.
	if len(flagsHex) != 2 {
		return trace.SpanContext{}, false
	}
	flagsBytes, err := hex.DecodeString(flagsHex)
	if err != nil {
		return trace.SpanContext{}, false
	}

	cfg := trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.TraceFlags(flagsBytes[0]),
		Remote:     true,
	}

	sc := trace.NewSpanContext(cfg)
	if !sc.IsValid() {
		log.Printf("gobservability: invalid span context from traceparent %q", tp)
		return trace.SpanContext{}, false
	}

	return sc, true
}
