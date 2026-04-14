package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gocache/api/command"
	apilogger "gocache/api/logger"
	"gocache/sdk/pluginsdk"
)

const (
	pluginName    = "gobservability"
	pluginVersion = "0.1.0"
	defaultPort   = ":9100"
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
	p.log.Info().Msg("shutting down")
	if p.tracer != nil {
		if err := p.tracer.Shutdown(ctx); err != nil {
			p.log.Error().Err(err).Msg("tracer shutdown error")
		}
	}
	return p.server.Shutdown(ctx)
}

// HookPlugin interface.

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

	// Create OTEL span if tracer is enabled.
	if p.tracer != nil {
		var startNs uint64
		if v, ok := req.Context[command.StartNs]; ok {
			startNs, _ = strconv.ParseUint(v, 10, 64)
		}
		p.tracer.RecordCommand(req.Command, req.Args, elapsedNs, startNs, isError, req.ResultError, req.Metadata)
	}

	return nil
}

// QueryPlugin interface.

func (p *gobservabilityPlugin) SetSession(s *pluginsdk.Session) {
	p.session = s
}

// ScopePlugin interface.

func (p *gobservabilityPlugin) Scopes() []string {
	return []string{"hook:post", "server:query:health", "server:query:plugins"}
}

func main() {
	port := os.Getenv("GOBSERVABILITY_PORT")
	if port == "" {
		port = defaultPort
	}

	plog := apilogger.New(os.Stdout, pluginName, "debug")

	collector := NewCollector()

	plugin := &gobservabilityPlugin{
		collector: collector,
		log:       plog,
	}

	// Initialize OTEL tracer if enabled.
	if otlpEndpoint := os.Getenv("GOBSERVABILITY_OTLP_ENDPOINT"); otlpEndpoint != "" {
		serviceName := os.Getenv("OTEL_SERVICE_NAME")
		if serviceName == "" {
			serviceName = "gocache"
		}
		tracer, err := NewTracer(otlpEndpoint, serviceName)
		if err != nil {
			plog.Error().Err(err).Msg("failed to initialize OTEL tracer")
		} else {
			plugin.tracer = tracer
			plog.Info().Str("endpoint", otlpEndpoint).Msg("OTEL tracing enabled")
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler(collector, pluginName, pluginVersion))
	mux.Handle("/healthz", healthzHandler(plugin))
	mux.Handle("/readyz", readyzHandler(plugin))

	httpServer := &http.Server{
		Addr:         port,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		plog.Info().Str("addr", port).Msg("metrics server listening")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			plog.Error().Err(err).Msg("metrics server error")
		}
	}()

	plugin.server = httpServer

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := pluginsdk.Run(ctx, plugin); err != nil {
		plog.Error().Err(err).Msg("plugin error")
		os.Exit(1)
	}
}
