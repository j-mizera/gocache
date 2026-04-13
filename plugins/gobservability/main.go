package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gocache/api/command"
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
}

// Plugin interface.

func (p *gobservabilityPlugin) Name() string    { return pluginName }
func (p *gobservabilityPlugin) Version() string { return pluginVersion }
func (p *gobservabilityPlugin) Critical() bool  { return false }

func (p *gobservabilityPlugin) OnHealthCheck(_ context.Context) error {
	return nil
}

func (p *gobservabilityPlugin) OnShutdown(ctx context.Context) error {
	log.Println("gobservability: shutting down")
	if p.tracer != nil {
		if err := p.tracer.Shutdown(ctx); err != nil {
			log.Printf("gobservability: tracer shutdown error: %v", err)
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

	collector := NewCollector()

	plugin := &gobservabilityPlugin{
		collector: collector,
	}

	// Initialize OTEL tracer if enabled.
	if otlpEndpoint := os.Getenv("GOBSERVABILITY_OTLP_ENDPOINT"); otlpEndpoint != "" {
		serviceName := os.Getenv("OTEL_SERVICE_NAME")
		if serviceName == "" {
			serviceName = "gocache"
		}
		tracer, err := NewTracer(otlpEndpoint, serviceName)
		if err != nil {
			log.Printf("gobservability: failed to initialize OTEL tracer: %v", err)
		} else {
			plugin.tracer = tracer
			log.Printf("gobservability: OTEL tracing enabled, exporting to %s", otlpEndpoint)
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
		log.Printf("gobservability: metrics server listening on %s", port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("gobservability: metrics server error: %v", err)
		}
	}()

	plugin.server = httpServer

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := pluginsdk.Run(ctx, plugin); err != nil {
		log.Printf("gobservability: plugin error: %v", err)
		os.Exit(1)
	}
}
