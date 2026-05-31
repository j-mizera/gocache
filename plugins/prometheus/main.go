package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	apiconfig "gocache/api/config"
	"gocache/api/scope"
	"gocache/api/version"
	apilogger "gocache/commons/logger"
	"gocache/sdk/pluginsdk"
)

const (
	pluginName = "prometheus"

	envPort = "PROMETHEUS_PORT"

	defaultPort     = ":9100"
	defaultLogLevel = "debug"

	httpReadTimeout  = 5 * time.Second
	httpWriteTimeout = 10 * time.Second
)

type prometheusPlugin struct {
	collector *Collector
	server    *http.Server
	session   *pluginsdk.Session
	log       *apilogger.Logger
}

func (p *prometheusPlugin) Name() string    { return pluginName }
func (p *prometheusPlugin) Version() string { return version.Version }
func (p *prometheusPlugin) Critical() bool  { return false }

func (p *prometheusPlugin) OnHealthCheck(_ context.Context) error {
	return nil
}

func (p *prometheusPlugin) OnShutdown(ctx context.Context) error {
	p.log.Info(ctx).Msg("shutting down")
	return p.server.Shutdown(ctx)
}

func (p *prometheusPlugin) SetSession(s *pluginsdk.Session) {
	p.session = s
}

func (p *prometheusPlugin) OnConfigReload(cfg apiconfig.PluginConfig) {
	p.log.InfoNoCtx().Msg("config reloaded")
}

func (p *prometheusPlugin) Scopes() []string {
	return []string{
		string(scope.ScopeServerQueryHealth),
		string(scope.ScopeServerQueryPlugins),
		string(scope.ScopeServerQueryMetricsCommands),
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	port := os.Getenv(envPort)
	if port == "" {
		port = defaultPort
	}

	plog := apilogger.New(os.Stdout, pluginName, defaultLogLevel)

	collector := NewCollector()

	plugin := &prometheusPlugin{
		collector: collector,
		log:       plog,
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler(plugin, pluginName, version.Version))
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

	if err := pluginsdk.Run(ctx, plugin); err != nil {
		plog.ErrorNoCtx().Err(err).Msg("plugin error")
		os.Exit(1)
	}
}
