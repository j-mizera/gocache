package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/evaluator"
	"gocache/pkg/logger"
	"gocache/pkg/plugin/hooks"
	"gocache/pkg/plugin/router"
	"gocache/pkg/resp"
	"gocache/pkg/watch"
)

type Server struct {
	addr             string
	cache            *cache.Cache
	engine           *engine.Engine
	evaluator        evaluator.Evaluator
	listener         net.Listener
	shutdownChan     chan struct{}
	connectionWg     sync.WaitGroup
	shutdownOnce     sync.Once
	isShuttingDown   bool
	mu               sync.RWMutex
	requirePass      string
	blockingRegistry *blocking.Registry
	watchManager     *watch.Manager
}

func New(addr string, c *cache.Cache, e *engine.Engine, snapshotFile, requirePass string, br *blocking.Registry, wm *watch.Manager) *Server {
	return &Server{
		addr:             addr,
		cache:            c,
		engine:           e,
		evaluator:        evaluator.New(c, e, snapshotFile, requirePass, br, wm),
		shutdownChan:     make(chan struct{}),
		requirePass:      requirePass,
		blockingRegistry: br,
		watchManager:     wm,
	}
}

// CoreCommandNames returns the list of core command names for plugin shadow checking.
func (srv *Server) CoreCommandNames() []string {
	return srv.evaluator.CoreCommandNames()
}

// SetPluginRouter sets the plugin command router on the evaluator.
func (srv *Server) SetPluginRouter(r *router.Router) {
	srv.evaluator.SetPluginRouter(r)
}

// SetHookExecutor sets the hook executor on the evaluator.
func (srv *Server) SetHookExecutor(e *hooks.Executor) {
	srv.evaluator.SetHookExecutor(e)
}

// Start begins accepting connections and blocks until shutdown
func (srv *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", srv.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", srv.addr, err)
	}
	srv.listener = listener

	logger.Info().Str("addr", srv.addr).Msg("server listening")

	// Accept connections in a goroutine
	go srv.acceptConnections()

	// Wait for shutdown signal or context cancellation
	select {
	case <-srv.shutdownChan:
		return nil
	case <-ctx.Done():
		srv.Shutdown(5 * time.Second)
		return ctx.Err()
	}
}

// acceptConnections handles the accept loop
func (srv *Server) acceptConnections() {
	for {
		conn, err := srv.listener.Accept()
		if err != nil {
			srv.mu.RLock()
			shuttingDown := srv.isShuttingDown
			srv.mu.RUnlock()

			if shuttingDown {
				return
			}
			logger.Error().Err(err).Msg("failed to accept connection")
			continue
		}

		srv.connectionWg.Add(1)
		go srv.handleConnection(conn)
	}
}

// Shutdown gracefully shuts down the server
func (srv *Server) Shutdown(timeout time.Duration) error {
	var err error
	srv.shutdownOnce.Do(func() {
		logger.Info().Msg("initiating graceful shutdown")

		// Mark as shutting down
		srv.mu.Lock()
		srv.isShuttingDown = true
		srv.mu.Unlock()

		// Stop accepting new connections
		if srv.listener != nil {
			if err := srv.listener.Close(); err != nil {
				logger.Warn().Err(err).Msg("listener close error")
			}
		}

		// Wait for existing connections to finish with timeout
		done := make(chan struct{})
		go func() {
			srv.connectionWg.Wait()
			close(done)
		}()

		select {
		case <-done:
			logger.Info().Msg("all connections closed gracefully")
		case <-time.After(timeout):
			logger.Warn().Msg("shutdown timeout reached, forcing close")
		}

		// Signal shutdown complete
		close(srv.shutdownChan)
	})
	return err
}

func (srv *Server) handleConnection(conn net.Conn) {
	ctx := clientctx.New()

	defer func() {
		if srv.watchManager != nil {
			srv.watchManager.Unwatch(ctx)
		}
		conn.Close()
		srv.connectionWg.Done()
	}()

	reader := resp.NewReader(conn)
	writer := resp.NewWriter(conn)
	defer writer.Flush()

	for {
		// Check if server is shutting down
		srv.mu.RLock()
		shuttingDown := srv.isShuttingDown
		srv.mu.RUnlock()

		if shuttingDown {
			_ = writer.Write(resp.MarshalError("ERR: Server is shutting down"))
			return
		}

		val, err := reader.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Debug().Err(err).Msg("connection read error")
			}
			return
		}

		if val.Type != resp.Array {
			if err := writer.Write(resp.MarshalError("ERR: Protocol error: expected array")); err != nil {
				return
			}
			if reader.Buffered() == 0 {
				if err := writer.Flush(); err != nil {
					return
				}
			}
			continue
		}

		if len(val.Array) == 0 {
			continue
		}

		parts := make([]string, len(val.Array))
		for i, v := range val.Array {
			parts[i] = v.Str
		}

		op := strings.ToUpper(parts[0])
		if op == "QUIT" {
			_ = writer.Write(resp.OK())
			return
		}

		// Auth gate: block commands until authenticated
		if srv.requirePass != "" && !ctx.Authenticated {
			if op != "AUTH" && op != "HELLO" {
				if err := writer.Write(resp.MarshalError("NOAUTH Authentication required.")); err != nil {
					return
				}
				if reader.Buffered() == 0 {
					if err := writer.Flush(); err != nil {
						return
					}
				}
				continue
			}
		}

		res := srv.evaluator.Evaluate(ctx, op, parts[1:])
		if err := writer.Write(srv.mapToResp(ctx, res)); err != nil {
			return
		}
		if reader.Buffered() == 0 {
			if err := writer.Flush(); err != nil {
				return
			}
		}
	}
}

func (srv *Server) mapToResp(ctx *clientctx.ClientContext, res evaluator.Result) resp.Value {
	if res.Err != nil {
		switch {
		case errors.Is(res.Err, resp.ErrWrongType):
			return resp.ErrWrongTypeValue()
		case errors.Is(res.Err, resp.ErrNotInteger):
			return resp.ErrNotIntegerValue()
		case errors.Is(res.Err, resp.ErrNotFloat):
			return resp.ErrNotFloatValue()
		default:
			return resp.MarshalError("ERR " + res.Err.Error())
		}
	}
	switch res.Value {
	case "OK":
		return resp.OK()
	case "QUEUED":
		return resp.Queued()
	}
	return srv.mapValueToResp(ctx, res.Value)
}

func (srv *Server) mapValueToResp(ctx *clientctx.ClientContext, val interface{}) resp.Value {
	proto := ctx.ProtoVersion

	switch v := val.(type) {
	case string:
		return resp.MarshalBulkString(v)
	case int:
		return resp.MarshalInt(v)
	case int64:
		return resp.MarshalInt(int(v))
	case float64:
		if proto >= 3 {
			return resp.MarshalDouble(v)
		}
		return resp.MarshalBulkString(fmt.Sprintf("%g", v))
	case []interface{}:
		respArray := make([]resp.Value, len(v))
		for i, item := range v {
			respArray[i] = srv.mapValueToResp(ctx, item)
		}
		return resp.ValueArray(respArray...)
	case []string:
		return resp.StringArray(v)
	case map[string]string:
		if proto >= 3 {
			pairs := make([]resp.Value, 0, len(v)*2)
			for key, value := range v {
				pairs = append(pairs, resp.MarshalBulkString(key), resp.MarshalBulkString(value))
			}
			return resp.MapFromPairs(pairs...)
		}
		// RESP2: flatten to alternating array
		arr := make([]resp.Value, 0, len(v)*2)
		for key, value := range v {
			arr = append(arr, resp.MarshalBulkString(key), resp.MarshalBulkString(value))
		}
		return resp.ValueArray(arr...)
	case map[string]interface{}:
		if proto >= 3 {
			pairs := make([]resp.Value, 0, len(v)*2)
			for key, value := range v {
				pairs = append(pairs, resp.MarshalBulkString(key), srv.mapValueToResp(ctx, value))
			}
			return resp.MapFromPairs(pairs...)
		}
		// RESP2: flatten to alternating array
		arr := make([]resp.Value, 0, len(v)*2)
		for key, value := range v {
			arr = append(arr, resp.MarshalBulkString(key), srv.mapValueToResp(ctx, value))
		}
		return resp.ValueArray(arr...)
	case resp.Value:
		return v
	case nil:
		if proto >= 3 {
			return resp.NullV3()
		}
		return resp.Nil()
	default:
		return resp.MarshalBulkString(fmt.Sprintf("%v", v))
	}
}
