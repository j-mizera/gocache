package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apicommand "gocache/api/command"
	"gocache/api/events"
	ops "gocache/api/operations"
	"gocache/commons/logger"
	"gocache/commons/resp"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/pipeline"
	"gocache/pkg/plugin/router"
	"gocache/pkg/rex"
	"gocache/pkg/watch"
)

// ctxCancelShutdownTimeout is the window granted to drain active connections
// when the server's context is cancelled (vs an explicit Shutdown call).
const ctxCancelShutdownTimeout = 5 * time.Second

const maxBatchSize = 128

type batchEntry struct {
	op       string
	args     []string
	shard    int
	readOnly bool
}

type pendingCmd struct {
	op   string
	args []string
}

type Server struct {
	addr             string
	cache            *cache.Cache
	engine           *engine.Engine
	pipeline         *pipeline.Pipeline
	listener         net.Listener
	shutdownChan     chan struct{}
	connectionWg     sync.WaitGroup
	shutdownOnce     sync.Once
	isShuttingDown   atomic.Bool
	requirePass      string
	blockingRegistry *blocking.Registry
	watchManager     *watch.Manager
	startTime        time.Time
	activeConns      atomic.Int64
	emitter          events.Emitter
	tracker          *serverOps.Tracker
	opHookExecutor   pipeline.OpHookExecutor
	connRegistry     *ConnectionRegistry
}

func New(addr string, c *cache.Cache, e *engine.Engine, requirePass string, br *blocking.Registry, wm *watch.Manager) *Server {
	tracker := serverOps.NewTracker()
	p := pipeline.New(c, e, requirePass, br, wm)
	p.SetTracker(tracker)
	return &Server{
		addr:             addr,
		cache:            c,
		engine:           e,
		pipeline:         p,
		shutdownChan:     make(chan struct{}),
		requirePass:      requirePass,
		blockingRegistry: br,
		watchManager:     wm,
		startTime:        time.Now(),
		emitter:          events.NoopEmitter{},
		tracker:          tracker,
		connRegistry:     NewConnectionRegistry(),
	}
}

// ConnRegistry returns the connection registry for plugin push access.
func (srv *Server) ConnRegistry() *ConnectionRegistry {
	return srv.connRegistry
}

// CoreCommandNames returns the list of core command names for plugin shadow checking.
func (srv *Server) CoreCommandNames() []string {
	return srv.pipeline.CoreCommandNames()
}

// SetPluginRouter sets the plugin command router on the pipeline.
func (srv *Server) SetPluginRouter(r *router.Router) {
	srv.pipeline.SetPluginRouter(r)
}

// SetHookExecutor sets the hook executor on the pipeline.
func (srv *Server) SetHookExecutor(e apicommand.HookExecutor) {
	srv.pipeline.SetHookExecutor(e)
}

// SetEmitter sets the event emitter on both the server and pipeline.
func (srv *Server) SetEmitter(e events.Emitter) {
	srv.emitter = e
	srv.pipeline.SetEmitter(e)
}

// SetTracker sets the operation tracker on both the server and pipeline.
func (srv *Server) SetTracker(t *serverOps.Tracker) {
	srv.tracker = t
	srv.pipeline.SetTracker(t)
}

// SetOpHookExecutor sets the operation hook executor on both the server and pipeline.
func (srv *Server) SetOpHookExecutor(e pipeline.OpHookExecutor) {
	srv.opHookExecutor = e
	srv.pipeline.SetOpHookExecutor(e)
}

// SetCommandMetrics wires compact command metrics recording into the pipeline.
func (srv *Server) SetCommandMetrics(r pipeline.CommandMetricsRecorder) {
	srv.pipeline.SetCommandMetricsRecorder(r)
}

// SetPersistenceFeed wires the persistence coordinator's mutation-feed
// hook into the pipeline so command.Dispatch can emit mutations to
// registered Sinks. Pass nil to disable the feed (the default).
func (srv *Server) SetPersistenceFeed(f command.MutationEmitter) {
	srv.pipeline.SetPersistenceFeed(f)
}

// RegisterEmbeddedCommand registers a plugin-provided command into
// the pipeline. See Pipeline.RegisterEmbeddedCommand for details.
func (srv *Server) RegisterEmbeddedCommand(name string, fn func(context.Context, []string) (any, error), spec apicommand.Spec) {
	srv.pipeline.RegisterEmbeddedCommand(name, fn, spec)
}

// ServerStateProvider methods — used by the plugin manager for server query responses.

func (srv *Server) IsShuttingDown() bool {
	return srv.isShuttingDown.Load()
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
			if srv.isShuttingDown.Load() {
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

		srv.isShuttingDown.Store(true)

		if srv.listener != nil {
			if cerr := srv.listener.Close(); cerr != nil {
				logger.WarnNoCtx().Err(cerr).Msg("listener close error")
			}
		}

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
			err = fmt.Errorf("shutdown timed out after %v", timeout)
		}

		close(srv.shutdownChan)
	})
	return err
}

func (srv *Server) handleConnection(serverCtx context.Context, conn net.Conn) {
	srv.activeConns.Add(1)
	defer srv.activeConns.Add(-1)

	remoteAddr := conn.RemoteAddr().String()
	connStart := time.Now()

	// Disable Nagle so single-command-per-RTT clients don't pay the kernel's
	// 40 ms delayed-ack stall on every command. Matches valkey/redis-server
	// default. Without this, redis-benchmark standard-mode collection writes
	// stall at ~1k rps. Errors are non-fatal.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			logger.WarnNoCtx().Err(err).Msg("set TCP_NODELAY failed")
		}
	}

	// Generate a stable connection ID, independent of the operation system.
	connID := clientctx.NextConnectionID()

	// Create connection operation and derive a connection-scoped ctx.
	connOp := srv.tracker.Start(ops.TypeConnection, "")
	connOp.Enrich(apicommand.RemoteAddrKey, remoteAddr)
	connOp.Enrich(apicommand.ConnectionIDKey, connID)
	connCtx := ops.WithContext(serverCtx, connOp)
	if srv.opHookExecutor != nil {
		srv.opHookExecutor.RunStartHooks(connCtx, connOp)
	}
	srv.emitter.Emit(events.NewConnectionOpen(remoteAddr, connID).WithOperationID(connOp.ID))

	ctx := clientctx.New()
	ctx.ConnectionID = connID
	ctx.RemoteAddr = remoteAddr
	ctx.OperationID = connOp.ID

	defer func() {
		srv.connRegistry.Unregister(connID)
		if srv.watchManager != nil {
			srv.watchManager.Unwatch(ctx)
		}
		conn.Close()
		srv.connectionWg.Done()
		connOp.Complete()
		if srv.opHookExecutor != nil {
			srv.opHookExecutor.RunCompleteHooks(connOp)
		}
		srv.emitter.Emit(events.NewConnectionClose(remoteAddr, uint64(time.Since(connStart).Nanoseconds()), connID).WithOperationID(connOp.ID))
		srv.tracker.Complete(connOp.ID)
	}()

	reader := resp.NewReader(conn)
	writer := resp.NewWriter(conn)
	connHandle := srv.connRegistry.Register(connID, writer)
	defer func() {
		if err := connHandle.Flush(); err != nil {
			logger.Debug(connCtx).Err(err).Msg("final flush on connection close")
		}
	}()

	// Per-command metadata accumulator. META lines fill this map;
	// the next non-META command consumes and clears it.
	var cmdMeta map[string]string
	var pending *pendingCmd

	for {
		if srv.isShuttingDown.Load() {
			if err := connHandle.WriteValue(resp.MarshalError("ERR Server is shutting down")); err != nil {
				logger.Debug(connCtx).Err(err).Msg("write shutdown notice failed")
			}
			return
		}

		var op string
		var args []string

		if pending != nil {
			op = pending.op
			args = pending.args
			pending = nil
		} else {
			val, err := reader.Read()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					logger.Debug(connCtx).Err(err).Msg("connection read error")
				}
				return
			}

			if val.Type != resp.Array {
				if err := connHandle.WriteValue(resp.MarshalError("ERR Protocol error: expected array")); err != nil {
					return
				}
				if reader.Buffered() == 0 {
					if err := connHandle.Flush(); err != nil {
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
			op = strings.ToUpper(parts[0])
			args = parts[1:]
		}

		// META accumulation: when REXV is negotiated and the command is META,
		// collect key-value into the per-command map. META is RESP-compliant:
		// it always produces a response (+OK on success, -ERR on failure).
		if ctx.RexVersion > 0 && op == resp.CmdMeta {
			key, value, err := rex.ParseMeta(args)
			if err != nil {
				if writeErr := connHandle.WriteValue(resp.MarshalError("ERR " + err.Error())); writeErr != nil {
					return
				}
				if reader.Buffered() == 0 {
					if flushErr := connHandle.Flush(); flushErr != nil {
						return
					}
				}
				cmdMeta = nil
				continue
			}
			if cmdMeta == nil {
				cmdMeta = make(map[string]string)
			}
			cmdMeta[key] = value
			if writeErr := connHandle.WriteValue(resp.OK()); writeErr != nil {
				return
			}
			if reader.Buffered() == 0 {
				if flushErr := connHandle.Flush(); flushErr != nil {
					return
				}
			}
			continue
		}

		if op == "QUIT" {
			if err := connHandle.WriteValue(resp.OK()); err != nil {
				logger.Debug(connCtx).Err(err).Msg("write QUIT ack failed")
			}
			return
		}

		// Auth gate: block commands until authenticated
		if srv.requirePass != "" && !ctx.Authenticated {
			if op != "AUTH" && op != "HELLO" {
				srv.emitter.Emit(events.NewAuthFailed(remoteAddr, op).WithOperationID(ctx.OperationID))
				if err := connHandle.WriteValue(resp.MarshalError("NOAUTH Authentication required.")); err != nil {
					return
				}
				if reader.Buffered() == 0 {
					if err := connHandle.Flush(); err != nil {
						return
					}
				}
				cmdMeta = nil
				continue
			}
		}

		if reader.Buffered() > 0 && srv.canBatch(ctx, op, args) {
			pending = srv.runBatch(connCtx, ctx, reader, connHandle, op, args, cmdMeta)
			cmdMeta = nil
		} else {
			ctx.CmdMeta = cmdMeta
			res := srv.pipeline.Evaluate(connCtx, ctx, op, args)
			ctx.CmdMeta = nil
			cmdMeta = nil
			if !res.SuppressResponse {
				if err := connHandle.WriteValue(srv.mapToResp(ctx, res)); err != nil {
					return
				}
			}
		}

		if reader.Buffered() == 0 && pending == nil {
			if err := connHandle.Flush(); err != nil {
				return
			}
		}
	}
}

func (srv *Server) canBatch(ctx *clientctx.ClientContext, op string, args []string) bool {
	if srv.requirePass != "" && !ctx.Authenticated {
		return false
	}
	if ctx.InTransaction {
		return false
	}
	switch op {
	case "QUIT", "AUTH", resp.CmdHello, resp.CmdMeta,
		resp.CmdMulti, resp.CmdExec, resp.CmdDiscard:
		return false
	}
	spec, ok := srv.pipeline.SpecFor(op)
	if !ok || spec.KeyArgIndex < 0 || spec.MultiKey {
		return false
	}
	return spec.KeyArgIndex < len(args)
}

func (srv *Server) runBatch(
	connCtx context.Context,
	ctx *clientctx.ClientContext,
	reader *resp.Reader,
	handle *ConnHandle,
	firstOp string,
	firstArgs []string,
	cmdMeta map[string]string,
) *pendingCmd {
	spec, _ := srv.pipeline.SpecFor(firstOp)
	batch := make([]batchEntry, 1, 16)
	batch[0] = batchEntry{
		op:       firstOp,
		args:     firstArgs,
		shard:    srv.cache.ShardIndexOf(firstArgs[spec.KeyArgIndex]),
		readOnly: spec.ReadOnly,
	}

	var overflow *pendingCmd

	for reader.Buffered() > 0 && len(batch) < maxBatchSize {
		val, err := reader.Read()
		if err != nil {
			break
		}
		if val.Type != resp.Array || len(val.Array) == 0 {
			if err := handle.WriteValue(resp.MarshalError("ERR Protocol error: expected array")); err != nil {
				break
			}
			continue
		}
		parts := make([]string, len(val.Array))
		for i, v := range val.Array {
			parts[i] = v.Str
		}
		nextOp := strings.ToUpper(parts[0])
		nextArgs := parts[1:]

		if !srv.canBatch(ctx, nextOp, nextArgs) {
			overflow = &pendingCmd{op: nextOp, args: nextArgs}
			break
		}

		nextSpec, _ := srv.pipeline.SpecFor(nextOp)
		batch = append(batch, batchEntry{
			op:       nextOp,
			args:     nextArgs,
			shard:    srv.cache.ShardIndexOf(nextArgs[nextSpec.KeyArgIndex]),
			readOnly: nextSpec.ReadOnly,
		})
	}

	shardMode := make(map[int]bool, len(batch))
	for _, e := range batch {
		if prev, ok := shardMode[e.shard]; ok {
			shardMode[e.shard] = prev && e.readOnly
		} else {
			shardMode[e.shard] = e.readOnly
		}
	}

	shardIDs := make([]int, 0, len(shardMode))
	for id := range shardMode {
		shardIDs = append(shardIDs, id)
	}
	sort.Ints(shardIDs)

	releases := make([]func(), len(shardIDs))
	for i, id := range shardIDs {
		if shardMode[id] {
			releases[i] = srv.engine.AcquireShardRO(id)
		} else {
			releases[i] = srv.engine.AcquireShard(id)
		}
	}

	results := make([]apicommand.Result, len(batch))
	ctx.CmdMeta = cmdMeta
	for i, e := range batch {
		results[i] = srv.pipeline.EvaluatePreLocked(connCtx, ctx, e.op, e.args)
		if i == 0 {
			ctx.CmdMeta = nil
		}
	}

	for i := len(releases) - 1; i >= 0; i-- {
		releases[i]()
	}

	for _, r := range results {
		if !r.SuppressResponse {
			if err := handle.WriteValue(srv.mapToResp(ctx, r)); err != nil {
				break
			}
		}
	}

	return overflow
}

func (srv *Server) mapToResp(ctx *clientctx.ClientContext, res apicommand.Result) resp.Value {
	if res.Err != nil {
		switch {
		case errors.Is(res.Err, apicommand.ErrWrongType):
			return resp.ErrWrongTypeValue()
		case errors.Is(res.Err, apicommand.ErrNotInteger):
			return resp.ErrNotIntegerValue()
		case errors.Is(res.Err, apicommand.ErrNotFloat):
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

func (srv *Server) mapValueToResp(ctx *clientctx.ClientContext, val any) resp.Value {
	proto := ctx.ProtoVersion

	switch v := val.(type) {
	case []byte:
		// The string conversion copies once per reply; future work could
		// let resp.Value hold []byte directly to skip this allocation on
		// the hot GET path.
		return resp.MarshalBulkString(string(v))
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
	case []any:
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
	case map[string]any:
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
