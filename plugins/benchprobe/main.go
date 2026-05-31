package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gocache/api/scope"
	"gocache/api/version"
	apilogger "gocache/commons/logger"
	"gocache/sdk/pluginsdk"
)

const (
	pluginName = "benchprobe"
	envPort    = "BENCHPROBE_PORT"

	defaultPort       = ":9200"
	defaultLogLevel   = "debug"
	httpReadTimeout   = 5 * time.Second
	httpWriteTimeout  = 10 * time.Second
	queryTimeout      = 2 * time.Second
	topicBenchStats   = "bench.stats"
	topicPluginIPC    = "plugin.ipc"
	queryParamReset   = "reset"
	queryParamInclude = "include"
)

type querySession interface {
	QueryServer(ctx context.Context, topic string, params map[string]string) (map[string]string, error)
}

type benchprobePlugin struct {
	server  *http.Server
	session querySession
	log     *apilogger.Logger
}

type snapshotResponse struct {
	BenchStats map[string]string `json:"bench_stats"`
	PluginIPC  map[string]string `json:"plugin_ipc,omitempty"`
}

func (p *benchprobePlugin) Name() string    { return pluginName }
func (p *benchprobePlugin) Version() string { return version.Version }
func (p *benchprobePlugin) Critical() bool  { return false }

func (p *benchprobePlugin) Scopes() []string {
	return []string{
		string(scope.ScopeServerQueryHealth),
		string(scope.ScopeForTopic(topicBenchStats)),
		string(scope.ScopeForTopic(topicPluginIPC)),
	}
}

func (p *benchprobePlugin) SetSession(s *pluginsdk.Session) {
	p.session = s
}

func (p *benchprobePlugin) OnHealthCheck(context.Context) error {
	if p.session == nil {
		return errors.New("benchprobe session not ready")
	}
	return nil
}

func (p *benchprobePlugin) OnShutdown(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	p.log.Info(ctx).Msg("shutting down")
	return p.server.Shutdown(ctx)
}

func (p *benchprobePlugin) snapshot(ctx context.Context, reset bool, includeIPC bool) (*snapshotResponse, error) {
	if p.session == nil {
		return nil, errors.New("benchprobe session not ready")
	}
	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	params := map[string]string{queryParamReset: "false"}
	if reset {
		params[queryParamReset] = "true"
	}
	benchStats, err := p.session.QueryServer(queryCtx, topicBenchStats, params)
	if err != nil {
		return nil, err
	}
	resp := &snapshotResponse{BenchStats: benchStats}
	if includeIPC {
		pluginIPC, err := p.session.QueryServer(queryCtx, topicPluginIPC, nil)
		if err != nil {
			return nil, err
		}
		resp.PluginIPC = pluginIPC
	}
	return resp, nil
}

func snapshotHandler(p *benchprobePlugin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reset := parseBool(r.URL.Query().Get(queryParamReset))
		includeIPC := true
		if include := strings.TrimSpace(r.URL.Query().Get(queryParamInclude)); include != "" {
			includeIPC = strings.Contains(include, "ipc") || strings.Contains(include, "all")
		}
		resp, err := p.snapshot(r.Context(), reset, includeIPC)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func readyzHandler(p *benchprobePlugin) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if p.session == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		data, err := p.session.QueryServer(ctx, "health", nil)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable", "error": err.Error()})
			return
		}
		if data["status"] != "ok" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(data)
	})
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
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
	plugin := &benchprobePlugin{log: plog}

	mux := http.NewServeMux()
	mux.Handle("/snapshot", snapshotHandler(plugin))
	mux.Handle("/readyz", readyzHandler(plugin))
	server := &http.Server{
		Addr:         port,
		Handler:      mux,
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
	}
	plugin.server = server

	go func() {
		plog.InfoNoCtx().Str("addr", port).Msg("benchprobe server listening")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			plog.ErrorNoCtx().Err(err).Msg("benchprobe server error")
		}
	}()

	if err := pluginsdk.Run(ctx, plugin); err != nil {
		plog.ErrorNoCtx().Err(err).Msg("plugin error")
		os.Exit(1)
	}
}

var _ pluginsdk.Plugin = (*benchprobePlugin)(nil)
var _ pluginsdk.ScopePlugin = (*benchprobePlugin)(nil)
var _ pluginsdk.QueryPlugin = (*benchprobePlugin)(nil)
