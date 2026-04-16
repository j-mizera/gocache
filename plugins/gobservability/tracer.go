package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	opctx "gocache/api/context"
	apilogger "gocache/api/logger"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracer wraps an OTEL TracerProvider and manages operation-based spans.
type Tracer struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	log      *apilogger.Logger

	mu       sync.Mutex
	inflight map[string]inflightSpan // operationID → span data
}

type inflightSpan struct {
	span      trace.Span
	startTime time.Time
}

// NewTracer creates a Tracer with an OTLP HTTP exporter.
func NewTracer(endpoint, serviceName string, log *apilogger.Logger) (*Tracer, error) {
	ctx := context.Background()

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
	}
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
		log:      log,
		inflight: make(map[string]inflightSpan),
	}, nil
}

// StartOperation creates a span for an operation. Reads traceparent from
// context (shared.traceparent or shared.rex.traceparent), or generates one.
// Returns the traceparent string to write back into the operation context.
func (t *Tracer) StartOperation(opID, opType string, opContext map[string]string) string {
	if t == nil {
		return ""
	}

	ctx := context.Background()

	// Look for existing traceparent: shared.traceparent (canonical) then shared.rex.traceparent (REX fallback).
	traceparent := opContext["shared.traceparent"]
	if traceparent == "" {
		traceparent = opContext["shared.rex.traceparent"]
	}

	if traceparent != "" {
		if sc, valid := parseTraceparent(traceparent); valid {
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		}
	}

	_, span := t.tracer.Start(ctx, "gocache."+opType,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("gocache.operation.id", opID),
			attribute.String("gocache.operation.type", opType),
		),
	)

	t.mu.Lock()
	t.inflight[opID] = inflightSpan{span: span, startTime: time.Now()}
	t.mu.Unlock()

	// Generate traceparent from the new span if none existed.
	sc := span.SpanContext()
	return fmt.Sprintf("00-%s-%s-01", sc.TraceID().String(), sc.SpanID().String())
}

// CompleteOperation finalizes the span for an operation.
// Context is redacted (secrets stripped) before adding as attributes.
func (t *Tracer) CompleteOperation(opID, status, failReason string, opContext map[string]string) {
	if t == nil {
		return
	}

	t.mu.Lock()
	entry, ok := t.inflight[opID]
	if ok {
		delete(t.inflight, opID)
	}
	t.mu.Unlock()

	if !ok {
		return
	}

	span := entry.span

	// Add non-secret context as span attributes.
	redacted := opctx.RedactSecrets(opContext)
	for k, v := range redacted {
		// Skip internal keys that are already set or too noisy.
		if strings.HasPrefix(k, "_") && k != "_command" {
			continue
		}
		span.SetAttributes(attribute.String(k, v))
	}

	if status == "failed" {
		span.SetStatus(codes.Error, failReason)
	} else {
		span.SetStatus(codes.Ok, "")
	}

	span.End()
}

// RecordLog attaches a log entry as a span event to the inflight operation span.
// If no inflight span exists for the operation, the log is dropped (the operation
// may have already completed).
func (t *Tracer) RecordLog(operationID, level, message string, fields map[string]string) {
	if t == nil || operationID == "" {
		return
	}

	t.mu.Lock()
	entry, ok := t.inflight[operationID]
	t.mu.Unlock()

	if !ok {
		return
	}

	// Redact secrets from fields before attaching to span.
	redacted := opctx.RedactSecrets(fields)
	attrs := []attribute.KeyValue{
		attribute.String("log.level", level),
	}
	for k, v := range redacted {
		attrs = append(attrs, attribute.String(k, v))
	}

	entry.span.AddEvent(message, trace.WithAttributes(attrs...))
}

// GenerateTraceparent creates a new W3C traceparent string.
func GenerateTraceparent() string {
	var traceID [16]byte
	var spanID [8]byte
	rand.Read(traceID[:])
	rand.Read(spanID[:])
	return fmt.Sprintf("00-%s-%s-01", hex.EncodeToString(traceID[:]), hex.EncodeToString(spanID[:]))
}

// Shutdown flushes pending spans and shuts down the exporter.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t == nil || t.provider == nil {
		return nil
	}
	return t.provider.Shutdown(ctx)
}

// parseTraceparent parses a W3C traceparent header.
func parseTraceparent(tp string) (trace.SpanContext, bool) {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		return trace.SpanContext{}, false
	}
	if parts[0] != "00" {
		return trace.SpanContext{}, false
	}

	traceIDHex := parts[1]
	spanIDHex := parts[2]
	flagsHex := parts[3]

	if len(traceIDHex) != 32 {
		return trace.SpanContext{}, false
	}
	traceIDBytes, err := hex.DecodeString(traceIDHex)
	if err != nil {
		return trace.SpanContext{}, false
	}
	var traceID trace.TraceID
	copy(traceID[:], traceIDBytes)

	if len(spanIDHex) != 16 {
		return trace.SpanContext{}, false
	}
	spanIDBytes, err := hex.DecodeString(spanIDHex)
	if err != nil {
		return trace.SpanContext{}, false
	}
	var spanID trace.SpanID
	copy(spanID[:], spanIDBytes)

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
		return trace.SpanContext{}, false
	}

	return sc, true
}
