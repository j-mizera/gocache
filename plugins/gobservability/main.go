package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gocache/api/command"
	gcpc "gocache/api/gcpc/v1"
	apilogger "gocache/api/logger"
	apiplugin "gocache/api/plugin"
	"gocache/sdk/pluginsdk"
)

const (
	pluginName    = "gobservability"
	pluginVersion = "0.1.0"

	// Environment variables consumed by the plugin.
	envPort         = "GOBSERVABILITY_PORT"
	envOTLPEndpoint = "GOBSERVABILITY_OTLP_ENDPOINT"
	envOTELService  = "OTEL_SERVICE_NAME"

	// Defaults — used when the corresponding env var is unset.
	defaultPort        = ":9100"
	defaultServiceName = "gocache"
	defaultLogLevel    = "debug"

	// HTTP timeouts for the /metrics /healthz /readyz server.
	httpReadTimeout  = 5 * time.Second
	httpWriteTimeout = 10 * time.Second
)

type gobservabilityPlugin struct {
	collector *Collector
	server    *http.Server
	session   *pluginsdk.Session
	tracer    *Tracer
	log       *apilogger.Logger
}

// Plugin interface.

func (p *gobservabilityPlugin) Name() string    { return pluginName }
func (p *gobservabilityPlugin) Version() string { return pluginVersion }
func (p *gobservabilityPlugin) Critical() bool  { return false }

func (p *gobservabilityPlugin) OnHealthCheck(_ context.Context) error {
	return nil
}

func (p *gobservabilityPlugin) OnShutdown(ctx context.Context) error {
	p.log.InfoNoCtx().Msg("shutting down")
	if p.tracer != nil {
		if err := p.tracer.Shutdown(ctx); err != nil {
			p.log.ErrorNoCtx().Err(err).Msg("tracer shutdown error")
		}
	}
	return p.server.Shutdown(ctx)
}

// HookPlugin interface — Prometheus metrics only (no OTEL spans).

func (p *gobservabilityPlugin) Hooks() []pluginsdk.HookDecl {
	return []pluginsdk.HookDecl{
		{Pattern: "*", Phase: pluginsdk.HookPhasePost},
	}
}

func (p *gobservabilityPlugin) HandleHook(_ context.Context, req *pluginsdk.HookRequest) *pluginsdk.HookResponse {
	if req.Phase != pluginsdk.HookPhasePost {
		return nil
	}

	var elapsedNs uint64
	if v, ok := req.Context[command.ElapsedNs]; ok {
		elapsedNs, _ = strconv.ParseUint(v, 10, 64)
	}

	isError := req.ResultError != ""
	p.collector.Record(req.Command, elapsedNs, isError)

	return nil
}

// OperationHookPlugin interface — OTEL tracing for ALL operations.

func (p *gobservabilityPlugin) OperationHooks() []pluginsdk.OperationHookDecl {
	return []pluginsdk.OperationHookDecl{
		{Type: "*", Priority: 10},
	}
}

func (p *gobservabilityPlugin) HandleOperationHook(_ context.Context, req *pluginsdk.OperationHookRequest) *pluginsdk.OperationHookResponse {
	if req.Phase == apiplugin.PhaseStart {
		return p.onOperationStart(req)
	}
	p.onOperationComplete(req)
	return nil
}

func (p *gobservabilityPlugin) onOperationStart(req *pluginsdk.OperationHookRequest) *pluginsdk.OperationHookResponse {
	if p.tracer == nil {
		return nil
	}

	// Start a span — tracer reads traceparent from context or generates one.
	traceparent := p.tracer.StartOperation(req.OperationID, req.OperationType, req.Context)

	// Write the canonical traceparent back so all downstream sees it.
	return &pluginsdk.OperationHookResponse{
		ContextValues: map[string]string{
			ctxKeyTraceparent: traceparent,
		},
	}
}

func (p *gobservabilityPlugin) onOperationComplete(req *pluginsdk.OperationHookRequest) {
	if p.tracer == nil {
		return
	}

	// An empty failReason signals a successful completion; a non-empty string
	// becomes the OTEL span's error description.
	failReason := req.Context[command.ErrorKey]

	p.tracer.CompleteOperation(req.OperationID, failReason, req.Context)
}

// EventPlugin interface — subscribe to log.entry events for OTEL log export.

func (p *gobservabilityPlugin) EventTypes() []string {
	return []string{"log.entry"}
}

func (p *gobservabilityPlugin) HandleEvent(_ context.Context, evt *gcpc.EventV1) {
	if p.tracer == nil {
		return
	}
	logEntry := evt.GetLogEntry()
	if logEntry == nil {
		return
	}
	p.tracer.RecordLog(evt.OperationId, logEntry.Level, logEntry.Message, logEntry.Fields)
}

// QueryPlugin interface.

func (p *gobservabilityPlugin) SetSession(s *pluginsdk.Session) {
	p.session = s
}

// ScopePlugin interface.

func (p *gobservabilityPlugin) Scopes() []string {
	return []string{
		"hook:post",
		"operation:hook",
		"events",
		"server:query:health",
		"server:query:plugins",
	}
}

func main() {
	port := os.Getenv(envPort)
	if port == "" {
		port = defaultPort
	}

	plog := apilogger.New(os.Stdout, pluginName, defaultLogLevel)

	collector := NewCollector()

	plugin := &gobservabilityPlugin{
		collector: collector,
		log:       plog,
	}

	// Initialize OTEL tracer if enabled.
	if otlpEndpoint := os.Getenv(envOTLPEndpoint); otlpEndpoint != "" {
		serviceName := os.Getenv(envOTELService)
		if serviceName == "" {
			serviceName = defaultServiceName
		}
		tracer, err := NewTracer(otlpEndpoint, serviceName, plog)
		if err != nil {
			plog.ErrorNoCtx().Err(err).Msg("failed to initialize OTEL tracer")
		} else {
			plugin.tracer = tracer
			plog.InfoNoCtx().Str("endpoint", otlpEndpoint).Msg("OTEL tracing enabled")
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler(collector, pluginName, pluginVersion))
	mux.Handle("/healthz", healthzHandler(plugin))
	mux.Handle("/readyz", readyzHandler(plugin))

	httpServer := &http.Server{
		Addr:         port,
		Handler:      mux,
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
	}

	go func() {
		plog.InfoNoCtx().Str("addr", port).Msg("metrics server listening")
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			plog.ErrorNoCtx().Err(err).Msg("metrics server error")
		}
	}()

	plugin.server = httpServer

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := pluginsdk.Run(ctx, plugin); err != nil {
		plog.ErrorNoCtx().Err(err).Msg("plugin error")
		os.Exit(1)
	}
}
