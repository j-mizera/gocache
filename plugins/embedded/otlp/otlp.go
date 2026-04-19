//go:build otlp_embedded

package otlp

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"gocache/api/logger"
	"gocache/pkg/config"
	"gocache/pkg/embedded"
)

// Env var overrides. Plugin reads them in BootInit, before config.Load.
const (
	envEndpoint    = "GOCACHE_EMBEDDED_OTLP_ENDPOINT"
	envService     = "GOCACHE_EMBEDDED_OTLP_SERVICE"
	envTimeoutMs   = "GOCACHE_EMBEDDED_OTLP_TIMEOUT_MS"
	envInsecure    = "GOCACHE_EMBEDDED_OTLP_INSECURE"
	envDisabled    = "GOCACHE_EMBEDDED_OTLP_DISABLED"
	defaultService = "gocache"
	defaultTimeout = 3 * time.Second
	shutdownGrace  = 2 * time.Second

	// Component marker on every exported span so Grafana can segment
	// embedded-plugin spans from IPC-plugin (gobservability) spans.
	attrComponent  = "gocache.component"
	componentValue = "embedded_otlp"
	tracerName     = "gocache.embedded.otlp"

	// Canonical span names.
	spanProcess      = "gocache.process"
	spanConfigLoaded = "gocache.config_loaded"
	spanShutdown     = "gocache.shutdown"
)

type plugin struct {
	endpoint string
	service  string
	insecure bool
	timeout  time.Duration
	disabled bool

	provider    *sdktrace.TracerProvider
	tracer      trace.Tracer
	processCtx  context.Context
	processSpan trace.Span
}

func init() {
	embedded.Register(&plugin{
		service: defaultService,
		timeout: defaultTimeout,
	})
}

func (p *plugin) Name() string { return "otlp_embedded" }

// BootInit resolves env-var config and opens the OTLP exporter. Emits a
// long-lived "process" span whose end-time is set in ProcessShutdown so
// the whole server lifetime appears as a single span in Grafana.
func (p *plugin) BootInit(ctx context.Context) error {
	p.applyEnv()
	if p.disabled || p.endpoint == "" {
		// Soft-disable when not configured. The plugin still registers
		// so users see it in the "embedded plugins loaded" log, but it
		// does nothing at runtime.
		return nil
	}

	initCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(p.endpoint)}
	if p.insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(initCtx, opts...)
	if err != nil {
		return fmt.Errorf("otlp exporter: %w", err)
	}

	res, err := resource.New(initCtx,
		resource.WithAttributes(
			semconv.ServiceName(p.service),
			attribute.String(attrComponent, componentValue),
		),
	)
	if err != nil {
		return fmt.Errorf("otlp resource: %w", err)
	}

	p.provider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	p.tracer = p.provider.Tracer(tracerName)

	// Open the process span. End() is deferred to ProcessShutdown so the
	// span duration covers the full process lifetime. On a panic,
	// ProcessShutdown still fires via main()'s deferred chain, giving the
	// exporter a final chance to flush.
	p.processCtx, p.processSpan = p.tracer.Start(context.Background(), spanProcess,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.Int("process.pid", os.Getpid()),
		),
	)
	p.processSpan.AddEvent("boot_init")
	return nil
}

// ConfigLoaded emits a short child span under the process span to mark
// the boundary. Optional endpoint override from config is NOT applied
// here — changing the exporter after BootInit is too risky (we'd need
// to drain the pending batcher first). Env-var-only wins.
func (p *plugin) ConfigLoaded(ctx context.Context, cfg *config.Config) error {
	if p.tracer == nil || p.processSpan == nil {
		return nil
	}
	_, span := p.tracer.Start(p.processCtx, spanConfigLoaded,
		trace.WithAttributes(
			attribute.String("config.log_level", cfg.Server.LogLevel),
			attribute.Int("config.port", cfg.Server.Port),
		),
	)
	span.End()
	p.processSpan.AddEvent("config_loaded")
	return nil
}

// ProcessShutdown finalizes the process span and force-flushes the
// exporter. This is the call that makes a panicking server still land in
// Grafana without a restart: main()'s deferred chain runs this during
// panic unwind, and ForceFlush pushes whatever's in the batcher out
// before the process exits.
func (p *plugin) ProcessShutdown(_ context.Context) error {
	if p.provider == nil {
		return nil
	}
	// Use a fresh context with a tight timeout — the main ctx may already
	// be cancelled, and we don't want a slow telemetry backend to delay
	// process exit.
	flushCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if p.processSpan != nil {
		p.processSpan.AddEvent("shutdown")
		p.processSpan.End()
	}
	// Emit a terminal "shutdown" span so the shutdown path is visible
	// even in dashboards that don't render long-lived parent spans well.
	if p.tracer != nil {
		_, span := p.tracer.Start(context.Background(), spanShutdown)
		span.End()
	}
	if err := p.provider.ForceFlush(flushCtx); err != nil {
		logger.Warn(flushCtx).Err(err).Msg("otlp embedded force-flush failed")
	}
	if err := p.provider.Shutdown(flushCtx); err != nil {
		return fmt.Errorf("otlp embedded shutdown: %w", err)
	}
	return nil
}

// applyEnv reads env overrides. Called once from BootInit.
func (p *plugin) applyEnv() {
	if v := os.Getenv(envEndpoint); v != "" {
		p.endpoint = v
	}
	if v := os.Getenv(envService); v != "" {
		p.service = v
	}
	if v := os.Getenv(envTimeoutMs); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.timeout = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv(envInsecure); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			p.insecure = parsed
		}
	}
	if v := os.Getenv(envDisabled); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			p.disabled = parsed
		}
	}
	// Heuristic: HTTP endpoints without TLS scheme default to insecure.
	if !p.insecure && !strings.HasPrefix(p.endpoint, "https") {
		p.insecure = true
	}
}

