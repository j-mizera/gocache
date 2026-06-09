package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	apiconfig "gocache/api/config"
	apictx "gocache/api/context"
	apiEvents "gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	"gocache/api/scope"
	"gocache/api/version"
	apilogger "gocache/commons/logger"
	"gocache/sdk/pluginsdk"
)

const (
	pluginName = "instrumentation"

	keyEndpoint  = "endpoint"
	keyService   = "service"
	keyTimeoutMs = "timeout_ms"
	keyInsecure  = "insecure"
	keyDisabled  = "disabled"

	envEndpoint  = "GOCACHE_INSTRUMENTATION_OTLP_ENDPOINT"
	envService   = "GOCACHE_INSTRUMENTATION_OTLP_SERVICE"
	envTimeoutMs = "GOCACHE_INSTRUMENTATION_OTLP_TIMEOUT_MS"
	envInsecure  = "GOCACHE_INSTRUMENTATION_OTLP_INSECURE"
	envDisabled  = "GOCACHE_INSTRUMENTATION_OTLP_DISABLED"

	defaultService = "gocache"
	defaultTimeout = 3 * time.Second
	shutdownGrace  = 2 * time.Second

	componentKey   = "gocache.component"
	componentValue = "runtime_instrumentation"
	tracerName     = "gocache.runtime.instrumentation"
	loggerName     = "gocache.runtime.logs"
)

type plugin struct {
	log *apilogger.Logger

	mu         sync.RWMutex
	settings   settings
	lastErr    error
	tracer     trace.Tracer
	logger     otellog.Logger
	traceProv  *sdktrace.TracerProvider
	logProv    *sdklog.LoggerProvider
	active     map[string]activeSpan
	propagator propagation.TextMapPropagator
}

type settings struct {
	endpoint string
	service  string
	timeout  time.Duration
	insecure bool
	disabled bool
}

type activeSpan struct {
	ctx  context.Context
	span trace.Span
}

func newPlugin(log *apilogger.Logger) *plugin {
	return &plugin{
		log: log,
		settings: settings{
			service: defaultService,
			timeout: defaultTimeout,
		},
		active:     make(map[string]activeSpan),
		propagator: propagation.TraceContext{},
	}
}

func (p *plugin) Name() string    { return pluginName }
func (p *plugin) Version() string { return version.Version }
func (p *plugin) Critical() bool  { return false }

func (p *plugin) Scopes() []string {
	return []string{string(scope.ScopeEvents)}
}

func (p *plugin) EventTypes() []string {
	return []string{
		string(apiEvents.OperationStarted),
		string(apiEvents.OperationCompleted),
		string(apiEvents.RuntimeLogBatch),
		string(apiEvents.ReplayGap),
	}
}

func (p *plugin) OnHealthCheck(context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastErr
}

func (p *plugin) OnShutdown(ctx context.Context) error {
	return p.shutdownProviders(ctx)
}

func (p *plugin) OnConfigReload(cfg apiconfig.PluginConfig) {
	cfg.SetDefault(keyEndpoint, "")
	cfg.SetDefault(keyService, defaultService)
	cfg.SetDefault(keyTimeoutMs, int(defaultTimeout.Milliseconds()))
	cfg.SetDefault(keyInsecure, false)
	cfg.SetDefault(keyDisabled, false)
	cfg.BindEnv(keyEndpoint, envEndpoint)
	cfg.BindEnv(keyService, envService)
	cfg.BindEnv(keyTimeoutMs, envTimeoutMs)
	cfg.BindEnv(keyInsecure, envInsecure)
	cfg.BindEnv(keyDisabled, envDisabled)

	next := settings{
		endpoint: strings.TrimSpace(cfg.GetString(keyEndpoint)),
		service:  strings.TrimSpace(cfg.GetString(keyService)),
		timeout:  time.Duration(cfg.GetInt(keyTimeoutMs)) * time.Millisecond,
		insecure: cfg.GetBool(keyInsecure),
		disabled: cfg.GetBool(keyDisabled),
	}
	if next.service == "" {
		next.service = defaultService
	}
	if next.timeout <= 0 {
		next.timeout = defaultTimeout
	}
	if !next.insecure && next.endpoint != "" && !strings.HasPrefix(next.endpoint, "https") {
		next.insecure = true
	}

	if err := p.applySettings(next); err != nil {
		p.log.ErrorNoCtx().Err(err).Msg("instrumentation otlp configuration failed")
	}
}

func (p *plugin) HandleEvent(ctx context.Context, evt *gcpc.EventV1) {
	if evt == nil {
		return
	}
	switch payload := evt.Data.(type) {
	case *gcpc.EventV1_OperationStart:
		p.handleOperationStart(ctx, evt.Timestamp, payload.OperationStart)
	case *gcpc.EventV1_OperationComplete:
		p.handleOperationComplete(ctx, evt.Timestamp, payload.OperationComplete)
	case *gcpc.EventV1_RuntimeLogBatch:
		p.handleRuntimeLogBatch(ctx, payload.RuntimeLogBatch)
	case *gcpc.EventV1_ReplayGap:
		p.handleReplayGap(ctx, evt.Timestamp, payload.ReplayGap)
	}
}

func (p *plugin) applySettings(next settings) error {
	p.mu.RLock()
	current := p.settings
	lastErr := p.lastErr
	p.mu.RUnlock()
	if current == next && lastErr == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), next.timeout)
	defer cancel()

	if err := p.shutdownProviders(ctx); err != nil {
		p.recordErr(fmt.Errorf("shutdown previous providers: %w", err))
	}

	p.mu.Lock()
	p.settings = next
	p.lastErr = nil
	p.mu.Unlock()

	if next.disabled || next.endpoint == "" {
		p.log.InfoNoCtx().Bool("disabled", next.disabled).Msg("instrumentation otlp exporter disabled")
		return nil
	}

	traceOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(next.endpoint)}
	logOpts := []otlploghttp.Option{otlploghttp.WithEndpoint(next.endpoint)}
	if next.insecure {
		traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
		logOpts = append(logOpts, otlploghttp.WithInsecure())
	}

	traceExporter, err := otlptracehttp.New(ctx, traceOpts...)
	if err != nil {
		return p.recordErr(fmt.Errorf("create trace exporter: %w", err))
	}
	logExporter, err := otlploghttp.New(ctx, logOpts...)
	if err != nil {
		return p.recordErr(fmt.Errorf("create log exporter: %w", err))
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(next.service),
			attribute.String(componentKey, componentValue),
		),
	)
	if err != nil {
		return p.recordErr(fmt.Errorf("create otlp resource: %w", err))
	}

	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	logProvider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)

	p.mu.Lock()
	p.traceProv = traceProvider
	p.logProv = logProvider
	p.tracer = traceProvider.Tracer(tracerName)
	p.logger = logProvider.Logger(loggerName)
	p.lastErr = nil
	p.mu.Unlock()

	p.log.InfoNoCtx().Str("endpoint", next.endpoint).Str("service", next.service).Msg("instrumentation otlp exporter configured")
	return nil
}

func (p *plugin) recordErr(err error) error {
	p.mu.Lock()
	p.lastErr = err
	p.mu.Unlock()
	return err
}

func (p *plugin) shutdownProviders(ctx context.Context) error {
	p.mu.Lock()
	traceProvider := p.traceProv
	logProvider := p.logProv
	active := p.active
	p.traceProv = nil
	p.logProv = nil
	p.tracer = nil
	p.logger = nil
	p.active = make(map[string]activeSpan)
	p.mu.Unlock()

	now := time.Now()
	for _, span := range active {
		span.span.End(trace.WithTimestamp(now))
	}

	var err error
	if traceProvider != nil {
		if flushErr := traceProvider.ForceFlush(ctx); flushErr != nil {
			err = flushErr
		}
		if shutdownErr := traceProvider.Shutdown(ctx); shutdownErr != nil && err == nil {
			err = shutdownErr
		}
	}
	if logProvider != nil {
		if flushErr := logProvider.ForceFlush(ctx); flushErr != nil && err == nil {
			err = flushErr
		}
		if shutdownErr := logProvider.Shutdown(ctx); shutdownErr != nil && err == nil {
			err = shutdownErr
		}
	}
	return err
}

func (p *plugin) handleOperationStart(ctx context.Context, timestamp uint64, payload *gcpc.OperationStartEventV1) {
	if payload == nil || payload.Id == "" {
		return
	}
	p.mu.RLock()
	tracer := p.tracer
	p.mu.RUnlock()
	if tracer == nil {
		return
	}

	startTime := eventTime(timestamp)
	parentCtx := p.parentContext(ctx, payload.Context)
	spanCtx, span := tracer.Start(parentCtx, spanName(payload.Type, payload.Context),
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(startTime),
		trace.WithAttributes(operationStartAttributes(payload)...),
	)

	p.mu.Lock()
	if old, ok := p.active[payload.Id]; ok {
		old.span.End(trace.WithTimestamp(startTime))
	}
	p.active[payload.Id] = activeSpan{ctx: spanCtx, span: span}
	p.mu.Unlock()
}

func (p *plugin) handleOperationComplete(ctx context.Context, timestamp uint64, payload *gcpc.OperationCompleteEventV1) {
	if payload == nil || payload.Id == "" {
		return
	}
	endTime := eventTime(timestamp)

	p.mu.Lock()
	active, ok := p.active[payload.Id]
	if ok {
		delete(p.active, payload.Id)
	}
	tracer := p.tracer
	p.mu.Unlock()
	if !ok {
		if tracer == nil {
			return
		}
		startTime := endTime.Add(-time.Duration(payload.ElapsedNs))
		parentCtx := p.parentContext(ctx, payload.Context)
		_, span := tracer.Start(parentCtx, spanName(payload.Type, payload.Context),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithTimestamp(startTime),
			trace.WithAttributes(operationCompleteAttributes(payload)...),
		)
		active = activeSpan{span: span}
	}

	attrs := operationCompleteAttributes(payload)
	active.span.SetAttributes(attrs...)
	if strings.EqualFold(payload.Status, "failed") || payload.FailReason != "" {
		active.span.SetStatus(codes.Error, payload.FailReason)
	}
	active.span.End(trace.WithTimestamp(endTime))
}

func (p *plugin) handleRuntimeLogBatch(ctx context.Context, payload *gcpc.RuntimeLogBatchEventV1) {
	if payload == nil || len(payload.Records) == 0 {
		return
	}
	p.mu.RLock()
	logger := p.logger
	p.mu.RUnlock()
	if logger == nil {
		return
	}
	for _, record := range payload.Records {
		if record == nil {
			continue
		}
		logger.Emit(p.logContext(ctx, record), logRecord(record))
	}
}

func (p *plugin) handleReplayGap(ctx context.Context, timestamp uint64, payload *gcpc.ReplayGapEventV1) {
	if payload == nil {
		return
	}
	p.mu.RLock()
	logger := p.logger
	p.mu.RUnlock()
	if logger == nil {
		return
	}
	record := otellog.Record{}
	record.SetTimestamp(eventTime(timestamp))
	record.SetObservedTimestamp(time.Now())
	record.SetSeverity(otellog.SeverityWarn)
	record.SetSeverityText("warn")
	record.SetBody(otellog.StringValue("event replay gap"))
	record.AddAttributes(
		otellog.String("subscriber", payload.Subscriber),
		otellog.Int64("dropped_count", int64(payload.DroppedCount)),
		otellog.Int64("skipped_operations", int64(payload.SkippedOperations)),
		otellog.Int64("dropped_records", int64(payload.DroppedRecords)),
		otellog.Int64("dropped_completed", int64(payload.DroppedCompleted)),
		otellog.Int64("invalid_handles", int64(payload.InvalidHandles)),
		otellog.Int64("window_ms", int64(payload.WindowMs)),
	)
	logger.Emit(ctx, record)
}

func (p *plugin) parentContext(ctx context.Context, fields map[string]string) context.Context {
	if traceparent := firstNonEmpty(fields[apictx.SharedTraceparent], fields[apictx.SharedRexTraceparent], fields["traceparent"]); traceparent != "" {
		return p.propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
	}
	return ctx
}

func (p *plugin) logContext(ctx context.Context, record *gcpc.RuntimeLogRecordV1) context.Context {
	if record.OperationId != "" {
		p.mu.RLock()
		active, ok := p.active[record.OperationId]
		p.mu.RUnlock()
		if ok {
			return active.ctx
		}
	}
	return p.parentContext(ctx, record.Fields)
}

func logRecord(input *gcpc.RuntimeLogRecordV1) otellog.Record {
	record := otellog.Record{}
	ts := eventTime(input.Timestamp)
	record.SetTimestamp(ts)
	record.SetObservedTimestamp(time.Now())
	record.SetSeverity(severity(input.Level))
	record.SetSeverityText(input.Level)
	record.SetBody(otellog.StringValue(input.Message))

	attrs := []otellog.KeyValue{
		otellog.String("gocache.log.source", input.Source),
	}
	if input.OperationId != "" {
		attrs = append(attrs, otellog.String("gocache.operation.id", input.OperationId))
	}
	if input.Caller != "" {
		attrs = append(attrs, otellog.String("code.filepath", input.Caller))
	}
	fields := apictx.RedactSecrets(input.Fields)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attrs = append(attrs, otellog.String(key, fields[key]))
	}
	record.AddAttributes(attrs...)
	return record
}

func operationStartAttributes(payload *gcpc.OperationStartEventV1) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("gocache.operation.id", payload.Id),
		attribute.String("gocache.operation.type", payload.Type),
	}
	if payload.ParentId != "" {
		attrs = append(attrs, attribute.String("gocache.operation.parent_id", payload.ParentId))
	}
	return appendContextAttributes(attrs, payload.Context)
}

func operationCompleteAttributes(payload *gcpc.OperationCompleteEventV1) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("gocache.operation.id", payload.Id),
		attribute.String("gocache.operation.type", payload.Type),
		attribute.Int64("gocache.operation.elapsed_ns", int64(payload.ElapsedNs)),
		attribute.String("gocache.operation.status", payload.Status),
	}
	if payload.FailReason != "" {
		attrs = append(attrs, attribute.String("gocache.operation.fail_reason", payload.FailReason))
	}
	return appendContextAttributes(attrs, payload.Context)
}

func appendContextAttributes(attrs []attribute.KeyValue, fields map[string]string) []attribute.KeyValue {
	fields = apictx.RedactSecrets(fields)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attrs = append(attrs, attribute.String("gocache.context."+key, fields[key]))
	}
	return attrs
}

func spanName(opType string, fields map[string]string) string {
	if opType == "command" {
		if command := fields["_command"]; command != "" {
			return "gocache.command." + strings.ToUpper(command)
		}
	}
	if opType == "" {
		return "gocache.operation"
	}
	return "gocache.operation." + opType
}

func eventTime(timestamp uint64) time.Time {
	if timestamp == 0 {
		return time.Now()
	}
	return time.Unix(0, int64(timestamp))
}

func severity(level string) otellog.Severity {
	switch strings.ToLower(level) {
	case "trace":
		return otellog.SeverityTrace
	case "debug":
		return otellog.SeverityDebug
	case "warn", "warning":
		return otellog.SeverityWarn
	case "error":
		return otellog.SeverityError
	case "fatal", "panic":
		return otellog.SeverityFatal
	case "info", "":
		return otellog.SeverityInfo
	default:
		return otellog.SeverityInfo
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	plog := apilogger.New(os.Stdout, pluginName, "debug")
	plugin := newPlugin(plog)
	if err := pluginsdk.Run(ctx, plugin); err != nil {
		plog.ErrorNoCtx().Err(err).Msg("plugin error")
		os.Exit(1)
	}
}

var _ pluginsdk.Plugin = (*plugin)(nil)
var _ pluginsdk.ScopePlugin = (*plugin)(nil)
var _ pluginsdk.EventPlugin = (*plugin)(nil)
var _ pluginsdk.ConfigPlugin = (*plugin)(nil)
