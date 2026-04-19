package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"gocache/api/command"
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

// OTEL + W3C Trace Context constants.
const (
	// tracerName identifies this plugin's OTEL tracer instrument.
	tracerName = "gocache.gobservability"

	// Resource + span attribute keys.
	attrComponent     = "gocache.component"
	attrOperationID   = "gocache.operation.id"
	attrOperationType = "gocache.operation.type"
	attrLogLevel      = "log.level"
	attrReplayed      = "gocache.replayed" // true when the span was reconstructed from a late subscribe

	// componentValue is the value stored under attrComponent.
	componentValue = "gobservability"

	// spanNamePrefix prefixes every span name with a stable vendor namespace.
	spanNamePrefix = "gocache."

	// W3C Trace Context format: version-traceID-spanID-flags. Version "00"
	// and flags "01" (sampled) are the only combination currently emitted.
	traceparentFormat  = "00-%s-%s-01"
	traceparentVersion = "00"

	// Context keys where an incoming traceparent may be found.
	// ctxKeyTraceparent is canonical; ctxKeyRexTraceparent is accepted as a
	// fallback so REX-provided trace context still links client traces.
	ctxKeyTraceparent    = "shared.traceparent"
	ctxKeyRexTraceparent = "shared.rex.traceparent"
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

// NewTracer creates a Tracer with an OTLP HTTP exporter. The caller's ctx is
// used for the OTLP exporter handshake and resource discovery so process-level
// cancellation can interrupt a hung endpoint during startup.
func NewTracer(ctx context.Context, endpoint, serviceName string, log *apilogger.Logger) (*Tracer, error) {
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
			attribute.String(attrComponent, componentValue),
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
		tracer:   tp.Tracer(tracerName),
		log:      log,
		inflight: make(map[string]inflightSpan),
	}, nil
}

// StartOperation creates a span for an operation. Reads traceparent from
// context (shared.traceparent or shared.rex.traceparent), or generates one.
// Returns the traceparent string to write back into the operation context.
//
// If replayed is true, startUnixNs anchors the span at the operation's
// actual wall-clock start and the span carries a gocache.replayed=true
// attribute so dashboards can distinguish reconstructed spans from live
// ones. A zero startUnixNs in replay mode falls back to time.Now() (the
// server would never emit zero for a real replayed op, but defaulting
// defensively avoids surprises from future SDK callers).
func (t *Tracer) StartOperation(opID, opType string, opContext map[string]string, replayed bool, startUnixNs int64) string {
	if t == nil {
		return ""
	}

	ctx := context.Background()

	// Look for an existing traceparent: ctxKeyTraceparent (canonical) then
	// ctxKeyRexTraceparent (REX fallback).
	traceparent := opContext[ctxKeyTraceparent]
	if traceparent == "" {
		traceparent = opContext[ctxKeyRexTraceparent]
	}

	if traceparent != "" {
		if sc, valid := parseTraceparent(traceparent); valid {
			ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
		}
	}

	spanStart := time.Now()
	spanOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String(attrOperationID, opID),
			attribute.String(attrOperationType, opType),
		),
	}
	if replayed {
		if startUnixNs > 0 {
			spanStart = time.Unix(0, startUnixNs)
		}
		spanOpts = append(spanOpts,
			trace.WithTimestamp(spanStart),
			trace.WithAttributes(attribute.Bool(attrReplayed, true)),
		)
	}

	_, span := t.tracer.Start(ctx, spanNamePrefix+opType, spanOpts...)

	t.mu.Lock()
	t.inflight[opID] = inflightSpan{span: span, startTime: spanStart}
	t.mu.Unlock()

	// Emit a traceparent from the new span if none existed.
	sc := span.SpanContext()
	return fmt.Sprintf(traceparentFormat, sc.TraceID().String(), sc.SpanID().String())
}

// CompleteOperation finalizes the span for an operation. An empty failReason
// sets span status OK; a non-empty value sets span status Error with the
// reason as description. Context is redacted (secrets stripped) before
// attributes are attached.
func (t *Tracer) CompleteOperation(opID, failReason string, opContext map[string]string) {
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
		if strings.HasPrefix(k, "_") && k != command.CommandKey {
			continue
		}
		span.SetAttributes(attribute.String(k, v))
	}

	if failReason != "" {
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
		attribute.String(attrLogLevel, level),
	}
	for k, v := range redacted {
		attrs = append(attrs, attribute.String(k, v))
	}

	entry.span.AddEvent(message, trace.WithAttributes(attrs...))
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
	if parts[0] != traceparentVersion {
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
