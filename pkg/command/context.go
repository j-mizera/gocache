// Package command provides server-side command types for GoCache.
package command

import (
	"context"

	apicommand "gocache/api/command"
	apiobs "gocache/api/observability"
	apipersistence "gocache/api/persistence"
	commonobs "gocache/commons/observability"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/transaction"
	"gocache/pkg/watch"
)

// MutationEmitter is the dispatcher-side hook into the persistence
// coordinator. The cache write path uses HasSinks to short-circuit
// emission when no sink is registered (~1ns atomic load); when at least
// one sink is registered, AllocateAndEmit is called inside the shard
// lock to allocate the LSN and push the mutation to per-sink buffers.
//
// Implemented by *pkg/persistence.Coordinator. Nil is a valid value —
// the dispatcher treats it the same as "no sinks".
type MutationEmitter interface {
	HasSinks() bool
	AllocateAndEmit(op, key string, args [][]byte) apipersistence.LSN
}

// Handler is a function that handles a single cache command.
type Handler func(ctx *Context) apicommand.Result

// Context carries all dependencies needed to execute a command.
//
// Context is request-scoped and short-lived: it is constructed per command
// by the evaluator and discarded when the handler returns. context.Context is
// kept only for cancellation/deadlines/shutdown propagation. Operation
// telemetry travels explicitly through OperationScope instead of the context.
type Context struct {
	ctx                     context.Context
	telemetry               commonobs.OperationScope
	operationTrackerManager *commonobs.SlotOperationTrackerManager

	Client           *clientctx.ClientContext
	Op               string
	Args             []string
	InBatch          bool
	ShardLocked      bool
	Engine           *engine.Engine
	Cache            *cache.Cache
	Transaction      *transaction.Manager
	BlockingRegistry *blocking.Registry
	WatchManager     *watch.Manager

	// Evaluator-level config, set by the pipeline before dispatch.
	RequirePass string

	// Sharding routing — set by the evaluator before invoking the handler
	// based on the command's Spec.KeyArgIndex / Spec.MultiKey.
	//
	//   Shard >= 0  — single-key command; Dispatch routes to that shard's
	//                 engine goroutine.
	//   Shard == -1 — keyless command (PING, AUTH, HELLO, MULTI, …); Dispatch
	//                 runs the closure inline without acquiring any cache lock.
	//   MultiKey    — multi-key command; Dispatch acquires every shard's
	//                 write lock (in id order) for cross-shard atomicity.
	//                 Shard is ignored when MultiKey is true.
	Shard    int
	MultiKey bool

	// TouchedShards is the sorted unique shard set the multi-key handler
	// will actually access. When non-empty, Dispatch acquires only those
	// shards via Engine.DispatchToShards instead of the bulk lock.
	// Multi-key handlers populate it before calling Dispatch — see
	// pkg/resp/handler/basic.go::HandleMset for the canonical pattern.
	// Empty (nil or len 0) means "lock everything", the safe default for
	// commands that touch the entire keyspace (FLUSHDB, KEYS, EXEC,
	// snapshot save/load) or where the touched set is dynamic enough
	// that pre-computing it isn't worth the complexity.
	TouchedShards []int

	// EvalFn re-enters the evaluator pipeline. Used by EXEC to execute
	// queued commands in a batch. parentCtx is the connection-scoped ctx
	// from the outer Evaluate call.
	EvalFn func(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string, inBatch bool) apicommand.Result

	// Spec is the command's classification, populated by the evaluator
	// from the command registration. Dispatch reads Spec.ReadOnly to
	// decide whether to wrap fn with persistence emission.
	Spec apicommand.Spec

	// Coordinator is the persistence mutation-feed entry point, set by
	// the evaluator from the server's coordinator instance. May be nil
	// when no persistence is configured. When non-nil, Dispatch wraps
	// non-read-only fn closures with HasSinks-gated emission inside the
	// shard lock so LSN allocation order matches lock order.
	Coordinator MutationEmitter
}

// CancellationContext returns the cancellation/deadline context for the command.
// Returns context.Background() if no context was set, so callers never receive nil.
func (c *Context) CancellationContext() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// SetCancellationContext assigns the cancellation/deadline context for the command.
func (c *Context) SetCancellationContext(ctx context.Context) {
	c.ctx = ctx
}

// Context is a compatibility alias for CancellationContext while downstream
// pipeline/cache/handler call sites migrate away from context-carried telemetry.
func (c *Context) Context() context.Context {
	return c.CancellationContext()
}

// SetContext is a compatibility alias for SetCancellationContext while
// downstream call sites migrate to explicit OperationScope telemetry.
func (c *Context) SetContext(ctx context.Context) {
	c.SetCancellationContext(ctx)
}

// SetTelemetry assigns the explicit operation telemetry scope for this command.
func (c *Context) SetTelemetry(scope commonobs.OperationScope) {
	c.telemetry = scope
}

// Telemetry returns the explicit operation telemetry scope for this command.
func (c *Context) Telemetry() commonobs.OperationScope {
	return c.telemetry
}

// SetOperationTrackerManager assigns the connection context-version owner for
// command handlers that intentionally mutate connection-scoped telemetry state.
func (c *Context) SetOperationTrackerManager(manager *commonobs.SlotOperationTrackerManager) {
	c.operationTrackerManager = manager
}

// UpdateConnectionTelemetryContext creates a new current connection-context
// version for future operations. The active operation keeps its already pinned
// start-time version; active-operation projection changes only through telemetry
// records submitted to that operation.
func (c *Context) UpdateConnectionTelemetryContext(pairs ...string) bool {
	if c.operationTrackerManager == nil || c.Client == nil || c.Client.ConnectionIdentity == 0 {
		return false
	}
	version := c.operationTrackerManager.UpdateConnectionContextStrings(c.Client.ConnectionIdentity, pairs...)
	return !version.IsZero()
}

// RemoveConnectionTelemetryContext creates a new current connection-context
// version without keys for future operations.
func (c *Context) RemoveConnectionTelemetryContext(keys ...string) bool {
	if c.operationTrackerManager == nil || c.Client == nil || c.Client.ConnectionIdentity == 0 {
		return false
	}
	version := c.operationTrackerManager.RemoveConnectionContextStrings(c.Client.ConnectionIdentity, keys...)
	return !version.IsZero()
}

// RecordTelemetry submits a telemetry record through the command scope.
func (c *Context) RecordTelemetry(record apiobs.TelemetryRecord) bool {
	return c.telemetry.Record(record)
}

// Log submits a log-request telemetry record through the command scope.
func (c *Context) Log(level apiobs.TelemetryLogLevel, message []byte) bool {
	return c.telemetry.Log(level, message)
}

// Reset zeroes every field so the *Context can be returned to a sync.Pool
// without leaking references. Pointer fields explicitly nilled — leaving
// them set would keep ClientContext / Engine / Cache / etc. alive past the
// connection lifetime they were borrowed from.
func (c *Context) Reset() {
	c.ctx = nil
	c.telemetry = commonobs.OperationScope{}
	c.operationTrackerManager = nil
	c.Client = nil
	c.Op = ""
	c.Args = nil
	c.InBatch = false
	c.ShardLocked = false
	c.Engine = nil
	c.Cache = nil
	c.Transaction = nil
	c.BlockingRegistry = nil
	c.WatchManager = nil
	c.RequirePass = ""
	c.Shard = 0
	c.MultiKey = false
	c.TouchedShards = nil
	c.EvalFn = nil
	c.Spec = apicommand.Spec{}
	c.Coordinator = nil
}

// Dispatch runs fn under the appropriate locking discipline for the
// command's sharding shape:
//
//	InBatch          — engine lock already held by an outer dispatcher
//	                   (e.g. EXEC), run inline.
//	MultiKey         — acquire every shard's write lock (Engine.Dispatch-
//	                   WithResult takes Cache bulk lock) and run fn under
//	                   that umbrella.
//	Shard < 0        — keyless command; no cache touch, run inline.
//	Shard >= 0       — single-key command; route to that shard's engine
//	                   goroutine via Engine.DispatchToShard.
//
// Wraps the result into a Result, propagating any error. If the engine
// is stopped or the command context is cancelled before fn runs, the
// returned Result carries that error.
func Dispatch(ctx *Context, fn func() any) apicommand.Result {
	// Wrap fn with persistence-feed emission when (a) the command writes
	// (Spec.ReadOnly is false) and (b) a coordinator is attached. The
	// HasSinks check inside the wrapper is what gates the per-command
	// cost: zero when no sink is registered, atomic-load + LSN-allocate +
	// channel-send when one is. The wrapper always runs inside the shard
	// lock so LSN allocation order matches lock order — critical for
	// replay correctness.
	fn = wrapWithEmission(ctx, fn)

	if ctx.InBatch || ctx.ShardLocked {
		return wrapInline(fn)
	}
	if ctx.MultiKey {
		if len(ctx.TouchedShards) > 0 {
			// Selective lock — multi-key handler told us which shards it
			// actually touches; lock only those instead of the bulk lock.
			res, err := ctx.Engine.DispatchToShards(ctx.CancellationContext(), ctx.TouchedShards, fn)
			return wrapDispatch(res, err)
		}
		// Bulk lock — multi-key handler touches every shard or didn't
		// pre-compute its set (FLUSHDB, KEYS, SCAN, EXEC, snapshot, …).
		res, err := ctx.Engine.DispatchWithResult(ctx.CancellationContext(), fn)
		return wrapDispatch(res, err)
	}
	if ctx.Shard < 0 {
		return wrapInline(fn)
	}
	if ctx.Spec.ReadOnly {
		res, err := ctx.Engine.DispatchToShardRO(ctx.CancellationContext(), ctx.Shard, fn)
		return wrapDispatch(res, err)
	}
	res, err := ctx.Engine.DispatchToShard(ctx.CancellationContext(), ctx.Shard, fn)
	return wrapDispatch(res, err)
}

// wrapWithEmission returns fn unchanged for read-only commands or when
// no coordinator is attached. For writes with a coordinator, returns a
// closure that runs fn, then (inside the same goroutine and the same
// lock the dispatcher acquired) emits a Mutation if at least one sink
// is registered AND fn's result is not an error.
//
// The closure does NOT pay the emission cost on three short-circuits:
//
//   - read-only command (compile-time skip — bare fn returned)
//   - no coordinator   (compile-time skip — bare fn returned)
//   - HasSinks == false (atomic load, ~1 ns) — runtime skip inside lock
func wrapWithEmission(ctx *Context, fn func() any) func() any {
	if ctx.Spec.ReadOnly || ctx.Coordinator == nil {
		return fn
	}
	return func() any {
		res := fn()
		if !ctx.Coordinator.HasSinks() {
			return res
		}
		if _, isErr := res.(error); isErr {
			return res
		}
		ctx.Coordinator.AllocateAndEmit(ctx.Op, primaryKey(ctx), argsAsBytes(ctx.Args))
		return res
	}
}

// primaryKey returns the command's primary key for routing/sharding hints
// in the Mutation. For keyless commands or out-of-range KeyArgIndex
// returns the empty string. For multi-key commands returns the first key
// (Args[KeyArgIndex] if valid, else Args[0] when Args is non-empty).
//
// Mutation.Args carries the full argument list, so a sink that needs every
// key (AOF replay, multi-key replication) reads from there. Mutation.Key
// is a routing hint, not the source of truth.
func primaryKey(ctx *Context) string {
	idx := ctx.Spec.KeyArgIndex
	if idx >= 0 && idx < len(ctx.Args) {
		return ctx.Args[idx]
	}
	if ctx.MultiKey && len(ctx.Args) > 0 {
		return ctx.Args[0]
	}
	return ""
}

// argsAsBytes converts the string-typed argument slice to [][]byte for
// the Mutation wire shape. Each conversion copies (Go strings are
// immutable). Verbatim zero-copy from the RESP parser is a future
// optimization once the dispatcher carries the raw RESP bytes through.
func argsAsBytes(args []string) [][]byte {
	if len(args) == 0 {
		return nil
	}
	out := make([][]byte, len(args))
	for i, a := range args {
		out[i] = []byte(a)
	}
	return out
}

func wrapInline(fn func() any) apicommand.Result {
	res := fn()
	if err, ok := res.(error); ok {
		return apicommand.Result{Err: err}
	}
	return apicommand.Result{Value: res}
}

func wrapDispatch(res any, err error) apicommand.Result {
	if err != nil {
		return apicommand.Result{Err: err}
	}
	if resultErr, ok := res.(error); ok {
		return apicommand.Result{Err: resultErr}
	}
	return apicommand.Result{Value: res}
}

// Registration bundles a command handler with its argument spec.
// Handler packages return maps of these so the evaluator pipeline can
// validate args without hardcoding spec knowledge.
type Registration struct {
	Handler Handler
	Spec    apicommand.Spec
}
