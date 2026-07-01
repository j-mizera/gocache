package main

import (
	"context"
	"fmt"
	"net"
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
	apiplugin "gocache/api/plugin"
	"gocache/api/scope"
	"gocache/api/version"
	apilogger "gocache/commons/logger"
	"gocache/commons/transport"
	"gocache/sdk/pluginsdk"
)

const (
	pluginName = "instrumentation"

	keyEndpoint  = "endpoint"
	keyService   = "service"
	keyTimeoutMs = "timeout_ms"
	keyInsecure  = "insecure"
	keyDisabled  = "disabled"

	envEndpoint     = "GOCACHE_INSTRUMENTATION_OTLP_ENDPOINT"
	envService      = "GOCACHE_INSTRUMENTATION_OTLP_SERVICE"
	envTimeoutMs    = "GOCACHE_INSTRUMENTATION_OTLP_TIMEOUT_MS"
	envInsecure     = "GOCACHE_INSTRUMENTATION_OTLP_INSECURE"
	envDisabled     = "GOCACHE_INSTRUMENTATION_OTLP_DISABLED"
	envTelemetryShm = "GOCACHE_TELEMETRY_SHM"

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
	return []string{string(scope.ScopeEvents), string(scope.ScopeTelemetry)}
}

func (p *plugin) EventTypes() []string {
	return []string{
		string(apiEvents.ConnectionOpen),
		string(apiEvents.ConnectionClose),
		string(apiEvents.PluginRegistered),
		string(apiEvents.PluginCrashed),
		string(apiEvents.PluginRestarted),
		string(apiEvents.PluginStarted),
		string(apiEvents.PluginStopped),
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

func (p *plugin) handleReconstructedOperation(ctx context.Context, operation *pluginsdk.ReconstructedOperation) {
	if operation == nil || operation.OperationID == "" {
		return
	}

	p.mu.RLock()
	tracer := p.tracer
	logger := p.logger
	p.mu.RUnlock()
	if tracer == nil && logger == nil {
		return
	}

	endTime := time.Now()
	startTime := endTime
	if operation.Elapsed > 0 {
		startTime = endTime.Add(-operation.Elapsed)
	}

	operationCtx := p.parentContext(ctx, operation.Context)
	spanCtx := operationCtx
	var operationSpan trace.Span
	if tracer != nil {
		spanCtx, operationSpan = tracer.Start(operationCtx, "gocache.operation.telemetry",
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithTimestamp(startTime),
			trace.WithAttributes(reconstructedOperationAttributes(operation)...),
		)
		p.recordCommandSpans(spanCtx, tracer, operation.Commands, startTime)
	}

	if logger != nil {
		for _, logEntry := range operation.Logs {
			logger.Emit(spanCtx, reconstructedLogRecord(operation.OperationID, operation.Context, logEntry))
		}
	}

	if operationSpan != nil {
		if strings.EqualFold(operation.Status, "failed") {
			operationSpan.SetStatus(codes.Error, operation.Status)
		}
		operationSpan.End(trace.WithTimestamp(endTime))
	}
}

func (p *plugin) recordCommandSpans(ctx context.Context, tracer trace.Tracer, commands []pluginsdk.ReconstructedCommand, operationStartTime time.Time) {
	commandStartTime := operationStartTime
	for _, commandEntry := range commands {
		commandEndTime := commandStartTime
		if commandEntry.Elapsed > 0 {
			commandEndTime = commandStartTime.Add(commandEntry.Elapsed)
		}
		_, commandSpan := tracer.Start(ctx, commandSpanName(commandEntry.Name),
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithTimestamp(commandStartTime),
			trace.WithAttributes(reconstructedCommandAttributes(commandEntry)...),
		)
		if commandEntry.Error != "" {
			commandSpan.SetStatus(codes.Error, commandEntry.Error)
		}
		commandSpan.End(trace.WithTimestamp(commandEndTime))
		commandStartTime = commandEndTime
	}
}

func reconstructedOperationAttributes(operation *pluginsdk.ReconstructedOperation) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("gocache.operation.id", operation.OperationID),
		attribute.String("gocache.operation.source", "telemetry"),
	}
	if operation.Elapsed > 0 {
		attrs = append(attrs, attribute.Int64("gocache.operation.elapsed_ns", operation.Elapsed.Nanoseconds()))
	}
	if operation.Status != "" {
		attrs = append(attrs, attribute.String("gocache.operation.status", operation.Status))
	}
	return appendContextAttributes(attrs, operation.Context)
}

func reconstructedCommandAttributes(commandEntry pluginsdk.ReconstructedCommand) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("gocache.command.name", commandEntry.Name),
		attribute.Int("gocache.command.arg_count", len(commandEntry.Args)),
	}
	if commandEntry.Elapsed > 0 {
		attrs = append(attrs, attribute.Int64("gocache.command.elapsed_ns", commandEntry.Elapsed.Nanoseconds()))
	}
	if commandEntry.Error != "" {
		attrs = append(attrs, attribute.String("gocache.command.status", "error"))
		attrs = append(attrs, attribute.String("gocache.command.error", commandEntry.Error))
	} else {
		attrs = append(attrs, attribute.String("gocache.command.status", "ok"))
	}
	return attrs
}

func reconstructedLogRecord(operationID string, fields map[string]string, logEntry pluginsdk.ReconstructedLog) otellog.Record {
	record := otellog.Record{}
	record.SetTimestamp(time.Now())
	record.SetObservedTimestamp(time.Now())
	record.SetSeverity(severity(logEntry.Level))
	record.SetSeverityText(logEntry.Level)
	record.SetBody(otellog.StringValue(logEntry.Message))

	attrs := []otellog.KeyValue{
		otellog.String("gocache.operation.id", operationID),
		otellog.String("gocache.log.source", "telemetry"),
	}
	if logEntry.Caller != "" {
		attrs = append(attrs, otellog.String("code.filepath", logEntry.Caller))
	}
	redactedFields := apictx.RedactSecrets(fields)
	keys := make([]string, 0, len(redactedFields))
	for key := range redactedFields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attrs = append(attrs, otellog.String(key, redactedFields[key]))
	}
	record.AddAttributes(attrs...)
	return record
}

func commandSpanName(commandName string) string {
	if commandName == "" {
		return "gocache.command"
	}
	return "gocache.command." + strings.ToUpper(commandName)
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

func runInstrumentationPlugin(ctx context.Context, instrumentation *plugin) error {
	sockPath := os.Getenv(apiplugin.EnvSocketPath)
	if sockPath == "" {
		return fmt.Errorf("%s not set", apiplugin.EnvSocketPath)
	}

	socketConn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial plugin socket: %w", err)
	}
	framedConn := transport.NewConn(socketConn)
	defer framedConn.Close()

	var handlerWg sync.WaitGroup
	defer handlerWg.Wait()

	registerEnvelope := &gcpc.EnvelopeV1{
		Version: gcpc.ProtocolVersion,
		Payload: &gcpc.EnvelopeV1_Register{Register: &gcpc.RegisterV1{
			Name:            instrumentation.Name(),
			Version:         instrumentation.Version(),
			Critical:        instrumentation.Critical(),
			RequestedScopes: instrumentation.Scopes(),
		}},
	}
	if err := framedConn.Send(registerEnvelope); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	registerAckEnvelope, err := framedConn.Recv()
	if err != nil {
		return fmt.Errorf("recv register ack: %w", err)
	}
	registerAck := registerAckEnvelope.GetRegisterAck()
	if registerAck == nil {
		return fmt.Errorf("expected RegisterAck, got different message")
	}
	if !registerAck.Accepted {
		return fmt.Errorf("registration rejected: %s", registerAck.Reason)
	}
	if len(registerAck.GrantedScopes) > 0 {
		instrumentation.log.InfoNoCtx().Strs("scopes", registerAck.GrantedScopes).Msg("granted scopes")
	}
	logDeniedScopes(instrumentation, registerAck.GrantedScopes)

	remoteCfg := pluginsdk.NewRemoteConfig(registerAck.Config)
	instrumentation.OnConfigReload(remoteCfg)

	ackConfirmations := make(chan uint64, 1)
	telemetryPlugin, err := startTelemetryPlugin(ctx, instrumentation, framedConn, ackConfirmations, registerAck.GrantedScopes, registerAck.Config)
	if err != nil {
		return err
	}
	defer func() {
		stopTelemetryPlugin(instrumentation, telemetryPlugin)
	}()

	if err := framedConn.Send(gcpc.NewEventSubscribe(instrumentation.EventTypes())); err != nil {
		return fmt.Errorf("send event subscribe: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		envelope, err := framedConn.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("recv: %w", err)
		}

		switch envelope.Payload.(type) {
		case *gcpc.EnvelopeV1_HealthCheck:
			healthErr := instrumentation.OnHealthCheck(ctx)
			status := ""
			if healthErr != nil {
				status = healthErr.Error()
			}
			if err := framedConn.Send(gcpc.NewHealthResponse(healthErr == nil, status)); err != nil {
				return fmt.Errorf("send health response: %w", err)
			}
		case *gcpc.EnvelopeV1_Shutdown:
			shutdownPayload := envelope.GetShutdown()
			deadline := time.Unix(0, int64(shutdownPayload.DeadlineNs))
			shutdownCtx, cancel := context.WithDeadline(ctx, deadline)
			stopTelemetryPlugin(instrumentation, telemetryPlugin)
			telemetryPlugin = nil
			_ = instrumentation.OnShutdown(shutdownCtx)
			cancel()
			if err := framedConn.Send(gcpc.NewShutdownAck()); err != nil {
				return fmt.Errorf("send shutdown ack: %w", err)
			}
			return nil
		case *gcpc.EnvelopeV1_Event:
			eventPayload := envelope.GetEvent()
			handlerWg.Add(1)
			go func() {
				defer handlerWg.Done()
				instrumentation.HandleEvent(ctx, eventPayload)
			}()
		case *gcpc.EnvelopeV1_TelemetryAck:
			telemetryAck := envelope.GetTelemetryAck()
			if telemetryAck != nil {
				signalTelemetryConfirmation(instrumentation, ackConfirmations, telemetryAck.GetConsumedOffset())
			}
		case *gcpc.EnvelopeV1_ConfigUpdate:
			configUpdate := envelope.GetConfigUpdate()
			if configUpdate == nil {
				continue
			}
			remoteCfg.Replace(configUpdate.Entries)
			instrumentation.OnConfigReload(remoteCfg)
			if telemetryPlugin == nil {
				startedTelemetryPlugin, startErr := startTelemetryPlugin(ctx, instrumentation, framedConn, ackConfirmations, registerAck.GrantedScopes, configUpdate.Entries)
				if startErr != nil {
					instrumentation.log.ErrorNoCtx().Err(startErr).Msg("instrumentation telemetry startup failed")
					instrumentation.recordErr(startErr)
				} else {
					telemetryPlugin = startedTelemetryPlugin
				}
			}
		}
	}
}

func startTelemetryPlugin(ctx context.Context, instrumentation *plugin, framedConn *transport.Conn, confirmations <-chan uint64, grantedScopes []string, serverConfig map[string]string) (*pluginsdk.TelemetryPlugin, error) {
	if !hasGrantedScope(grantedScopes, string(scope.ScopeTelemetry)) {
		return nil, nil
	}

	filePath := firstNonEmpty(os.Getenv(envTelemetryShm), serverConfig[envTelemetryShm])
	if filePath == "" {
		return nil, fmt.Errorf("telemetry scope granted but %s is not configured", envTelemetryShm)
	}

	ackFunc := func(consumedOffset uint64) {
		if err := framedConn.Send(telemetryAckEnvelope(consumedOffset)); err != nil {
			instrumentation.log.ErrorNoCtx().Err(err).Uint64("consumed_offset", consumedOffset).Msg("failed to send telemetry ack")
		}
	}
	waitForConfirm := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-confirmations:
			if !ok {
				return fmt.Errorf("telemetry ack confirmation channel closed")
			}
			return nil
		}
	}

	telemetryPlugin := pluginsdk.NewTelemetryPlugin(filePath, ackFunc, waitForConfirm)
	if err := telemetryPlugin.Start(); err != nil {
		return nil, err
	}
	go consumeTelemetryOperations(ctx, instrumentation, telemetryPlugin.Operations())
	instrumentation.log.InfoNoCtx().Str("file", filePath).Msg("instrumentation telemetry plugin started")
	return telemetryPlugin, nil
}

func consumeTelemetryOperations(ctx context.Context, instrumentation *plugin, operations <-chan *pluginsdk.ReconstructedOperation) {
	for operation := range operations {
		instrumentation.handleReconstructedOperation(ctx, operation)
	}
}

func stopTelemetryPlugin(instrumentation *plugin, telemetryPlugin *pluginsdk.TelemetryPlugin) {
	if telemetryPlugin == nil {
		return
	}
	if err := telemetryPlugin.Stop(); err != nil {
		instrumentation.log.ErrorNoCtx().Err(err).Msg("instrumentation telemetry plugin stop failed")
	}
}

func telemetryAckEnvelope(consumedOffset uint64) *gcpc.EnvelopeV1 {
	return &gcpc.EnvelopeV1{
		Version: gcpc.ProtocolVersion,
		Payload: &gcpc.EnvelopeV1_TelemetryAck{TelemetryAck: &gcpc.TelemetryAck{ConsumedOffset: consumedOffset}},
	}
}

func signalTelemetryConfirmation(instrumentation *plugin, confirmations chan<- uint64, consumedOffset uint64) {
	select {
	case confirmations <- consumedOffset:
	default:
		instrumentation.log.WarnNoCtx().Uint64("consumed_offset", consumedOffset).Msg("dropping stale telemetry ack confirmation")
	}
}

func hasGrantedScope(grantedScopes []string, wantedScope string) bool {
	for _, grantedScope := range grantedScopes {
		if grantedScope == wantedScope {
			return true
		}
	}
	return false
}

func logDeniedScopes(instrumentation *plugin, grantedScopes []string) {
	grantedSet := make(map[string]struct{}, len(grantedScopes))
	for _, grantedScope := range grantedScopes {
		grantedSet[grantedScope] = struct{}{}
	}
	deniedScopes := make([]string, 0)
	for _, requestedScope := range instrumentation.Scopes() {
		if _, granted := grantedSet[requestedScope]; !granted {
			deniedScopes = append(deniedScopes, requestedScope)
		}
	}
	if len(deniedScopes) > 0 {
		instrumentation.log.WarnNoCtx().Strs("denied", deniedScopes).Msg("scopes denied — features requiring these scopes will return errors")
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	plog := apilogger.New(os.Stdout, pluginName, "debug")
	plugin := newPlugin(plog)
	if err := runInstrumentationPlugin(ctx, plugin); err != nil {
		plog.ErrorNoCtx().Err(err).Msg("plugin error")
		os.Exit(1)
	}
}

var _ pluginsdk.Plugin = (*plugin)(nil)
var _ pluginsdk.ScopePlugin = (*plugin)(nil)
var _ pluginsdk.EventPlugin = (*plugin)(nil)
var _ pluginsdk.ConfigPlugin = (*plugin)(nil)
