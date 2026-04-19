package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gocache/api/events"
	"gocache/api/logger"
	ops "gocache/api/operations"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/evaluator"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/plugin/router"
	"gocache/pkg/resp"
	"gocache/pkg/rex"
	"gocache/pkg/watch"
)

// ctxCancelShutdownTimeout is the window granted to drain active connections
// when the server's context is cancelled (vs an explicit Shutdown call).
const ctxCancelShutdownTimeout = 5 * time.Second

type Server struct {
	addr             string
	cache            *cache.Cache
	engine           *engine.Engine
	evaluator        *evaluator.BaseEvaluator
	listener         net.Listener
	shutdownChan     chan struct{}
	connectionWg     sync.WaitGroup
	shutdownOnce     sync.Once
	isShuttingDown   bool
	mu               sync.RWMutex
	requirePass      string
	blockingRegistry *blocking.Registry
	watchManager     *watch.Manager
	startTime        time.Time
	activeConns      atomic.Int64
	emitter          events.Emitter
	tracker          *serverOps.Tracker
	opHookExecutor   evaluator.OpHookExecutor
}

func New(addr string, c *cache.Cache, e *engine.Engine, snapshotFile, requirePass string, br *blocking.Registry, wm *watch.Manager) *Server {
	tracker := serverOps.NewTracker()
	ev := evaluator.New(c, e, snapshotFile, requirePass, br, wm)
	ev.SetTracker(tracker)
	return &Server{
		addr:             addr,
		cache:            c,
		engine:           e,
		evaluator:        ev,
		shutdownChan:     make(chan struct{}),
		requirePass:      requirePass,
		blockingRegistry: br,
		watchManager:     wm,
		startTime:        time.Now(),
		emitter:          events.NoopEmitter{},
		tracker:          tracker,
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
func (srv *Server) SetHookExecutor(e command.HookExecutor) {
	srv.evaluator.SetHookExecutor(e)
}

// SetEmitter sets the event emitter on both the server and evaluator.
func (srv *Server) SetEmitter(e events.Emitter) {
	srv.emitter = e
	srv.evaluator.SetEmitter(e)
}

// SetTracker sets the operation tracker on both the server and evaluator.
func (srv *Server) SetTracker(t *serverOps.Tracker) {
	srv.tracker = t
	srv.evaluator.SetTracker(t)
}

// SetOpHookExecutor sets the operation hook executor on both the server and evaluator.
func (srv *Server) SetOpHookExecutor(e evaluator.OpHookExecutor) {
	srv.opHookExecutor = e
	srv.evaluator.SetOpHookExecutor(e)
}

// EmitEvent emits an event through the server's emitter.
func (srv *Server) EmitEvent(evt events.Event) {
	srv.emitter.Emit(evt)
}

// ServerStateProvider methods — used by the plugin manager for server query responses.

func (srv *Server) IsShuttingDown() bool {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	return srv.isShuttingDown
}

func (srv *Server) StartTime() time.Time   { return srv.startTime }
func (srv *Server) ActiveConnections() int { return int(srv.activeConns.Load()) }
func (srv *Server) CacheKeys() int         { return srv.cache.Len() }
func (srv *Server) CacheUsedBytes() int64  { return srv.cache.UsedBytes() }
func (srv *Server) CacheMaxBytes() int64   { return srv.cache.MaxBytes() }

// Start begins accepting connections and blocks until shutdown
func (srv *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", srv.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", srv.addr, err)
	}
	srv.listener = listener

	logger.InfoNoCtx().Str("addr", srv.addr).Msg("server listening")

	// Accept connections in a goroutine; propagate the server lifecycle ctx.
	go srv.acceptConnections(ctx)

	// Wait for shutdown signal or context cancellation
	select {
	case <-srv.shutdownChan:
		return nil
	case <-ctx.Done():
		srv.Shutdown(ctxCancelShutdownTimeout)
		return ctx.Err()
	}
}

// acceptConnections handles the accept loop
func (srv *Server) acceptConnections(ctx context.Context) {
	for {
		conn, err := srv.listener.Accept()
		if err != nil {
			srv.mu.RLock()
			shuttingDown := srv.isShuttingDown
			srv.mu.RUnlock()

			if shuttingDown {
				return
			}
			logger.ErrorNoCtx().Err(err).Msg("failed to accept connection")
			continue
		}

		srv.connectionWg.Add(1)
		go srv.handleConnection(ctx, conn)
	}
}

// Shutdown gracefully shuts down the server
func (srv *Server) Shutdown(timeout time.Duration) error {
	var err error
	srv.shutdownOnce.Do(func() {
		logger.InfoNoCtx().Msg("initiating graceful shutdown")

		// Mark as shutting down
		srv.mu.Lock()
		srv.isShuttingDown = true
		srv.mu.Unlock()

		// Stop accepting new connections
		if srv.listener != nil {
			if err := srv.listener.Close(); err != nil {
				logger.WarnNoCtx().Err(err).Msg("listener close error")
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
			logger.InfoNoCtx().Msg("all connections closed gracefully")
		case <-time.After(timeout):
			logger.WarnNoCtx().Msg("shutdown timeout reached, forcing close")
		}

		// Signal shutdown complete
		close(srv.shutdownChan)
	})
	return err
}

func (srv *Server) handleConnection(serverCtx context.Context, conn net.Conn) {
	srv.activeConns.Add(1)
	defer srv.activeConns.Add(-1)

	remoteAddr := conn.RemoteAddr().String()
	connStart := time.Now()

	// Create connection operation and derive a connection-scoped ctx.
	connOp := srv.tracker.Start(ops.TypeConnection, "")
	connOp.Enrich(command.RemoteAddrKey, remoteAddr)
	connCtx := ops.WithContext(serverCtx, connOp)
	if srv.opHookExecutor != nil {
		srv.opHookExecutor.RunStartHooks(connCtx, connOp)
	}
	srv.emitter.Emit(events.NewConnectionOpen(remoteAddr).WithOperationID(connOp.ID))

	ctx := clientctx.New()
	ctx.OperationID = connOp.ID

	defer func() {
		if srv.watchManager != nil {
			srv.watchManager.Unwatch(ctx)
		}
		conn.Close()
		srv.connectionWg.Done()
		connOp.Complete()
		if srv.opHookExecutor != nil {
			srv.opHookExecutor.RunCompleteHooks(connOp)
		}
		srv.emitter.Emit(events.NewConnectionClose(remoteAddr, uint64(time.Since(connStart).Nanoseconds())).WithOperationID(connOp.ID))
		srv.tracker.Complete(connOp.ID)
	}()

	reader := resp.NewReader(conn)
	writer := resp.NewWriter(conn)
	defer writer.Flush()

	// Per-command metadata accumulator. META lines fill this map;
	// the next non-META command consumes and clears it.
	var cmdMeta map[string]string

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
				logger.DebugNoCtx().Err(err).Msg("connection read error")
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

		// META accumulation: when REXV is negotiated and the command is META,
		// collect key-value into the per-command map. META is RESP-compliant:
		// it always produces a response (+OK on success, -ERR on failure).
		if ctx.RexVersion > 0 && op == resp.CmdMeta {
			key, value, err := rex.ParseMeta(parts[1:])
			if err != nil {
				if writeErr := writer.Write(resp.MarshalError("ERR " + err.Error())); writeErr != nil {
					return
				}
				if reader.Buffered() == 0 {
					if flushErr := writer.Flush(); flushErr != nil {
						return
					}
				}
				cmdMeta = nil // discard accumulated metadata on error
				continue
			}
			if cmdMeta == nil {
				cmdMeta = make(map[string]string)
			}
			cmdMeta[key] = value
			if writeErr := writer.Write(resp.OK()); writeErr != nil {
				return
			}
			if reader.Buffered() == 0 {
				if flushErr := writer.Flush(); flushErr != nil {
					return
				}
			}
			continue
		}

		if op == "QUIT" {
			_ = writer.Write(resp.OK())
			return
		}

		// Auth gate: block commands until authenticated
		if srv.requirePass != "" && !ctx.Authenticated {
			if op != "AUTH" && op != "HELLO" {
				srv.emitter.Emit(events.NewAuthFailed(remoteAddr, op).WithOperationID(ctx.OperationID))
				if err := writer.Write(resp.MarshalError("NOAUTH Authentication required.")); err != nil {
					return
				}
				if reader.Buffered() == 0 {
					if err := writer.Flush(); err != nil {
						return
					}
				}
				cmdMeta = nil
				continue
			}
		}

		ctx.CmdMeta = cmdMeta
		res := srv.evaluator.Evaluate(connCtx, ctx, op, parts[1:])
		ctx.CmdMeta = nil
		cmdMeta = nil
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

func (srv *Server) mapToResp(ctx *clientctx.ClientContext, res command.Result) resp.Value {
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
