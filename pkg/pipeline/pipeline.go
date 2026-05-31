package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	apicommand "gocache/api/command"
	"gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	ops "gocache/api/operations"
	"gocache/commons/logger"
	"gocache/commons/resp"
	"gocache/pkg/benchstats"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/plugin/router"
	resphandler "gocache/pkg/resp/handler"
	"gocache/pkg/rex"
	rexhandler "gocache/pkg/rex/handler"
	"gocache/pkg/transaction"
	"gocache/pkg/watch"
)

// pluginCommandTimeout is the maximum time to wait for a plugin to respond.
const pluginCommandTimeout = 10 * time.Second

// cmdCtxPool recycles the per-command *command.Context. Reset zeroes every
// field on Put so a stale pointer cannot keep a ClientContext or any
// borrowed engine/cache pointer alive past its owning connection's life.
//
// Safety: cmdCtx is request-scoped — handlers run synchronously, blocking
// list ops (BLPOP/BRPOP) wait on a channel inside the same goroutine, and
// EXEC re-enters the evaluator (which gets its own fresh cmdCtx for the
// inner command). No handler captures cmdCtx in a goroutine that outlives
// the call.
var cmdCtxPool = sync.Pool{
	New: func() any { return &command.Context{} },
}

// putCmdCtx is a free function so `defer putCmdCtx(c)` records a function
// pointer + one arg rather than allocating a closure on the heap. The
// per-command alloc count is what this pool exists to drive down — using a
// closure here would re-introduce one of the allocations we are trying to
// remove.
func putCmdCtx(c *command.Context) {
	c.Reset()
	cmdCtxPool.Put(c)
}

// OpHookExecutor is the interface the evaluator uses to dispatch operation hooks.
// Defined here to avoid an import cycle with pkg/plugin/ophooks.
type OpHookExecutor interface {
	HasAny() bool
	HasOperationType(opType ops.Type) bool
	RunStartHooks(ctx context.Context, op *ops.Operation)
	RunCompleteHooks(op *ops.Operation)
}

// CommandMetricsRecorder records compact command-completion metrics without
// requiring full event payload construction.
type CommandMetricsRecorder interface {
	HasCommandMetricsSink() bool
	RecordCommand(command string, elapsedNs uint64, isError bool)
}

// Pipeline is the command dispatch pipeline. It owns no command-specific
// knowledge — handlers and their argument specs are provided by external
// packages (resp/handler, rex/handler) via command.Registration.
//
// Following "accept interfaces, return structs," constructors return
// *Pipeline directly; consumers that need a narrower surface for testing
// should define their own interface locally (pkg/server does this via a
// package-private alias of the methods it actually calls).
type Pipeline struct {
	cache              *cache.Cache
	engine             *engine.Engine
	transactionManager *transaction.Manager
	handlers           map[string]command.Handler
	specs              map[string]apicommand.Spec
	requirePass        string
	blockingRegistry   *blocking.Registry
	watchManager       *watch.Manager
	pluginRouter       *router.Router
	hookExecutor       apicommand.HookExecutor
	emitter            events.Emitter
	tracker            *serverOps.Tracker
	opHookExecutor     OpHookExecutor
	commandMetrics     CommandMetricsRecorder
	persistenceFeed    command.MutationEmitter
}

func New(c *cache.Cache, e *engine.Engine, requirePass string, br *blocking.Registry, wm *watch.Manager) *Pipeline {
	b := &Pipeline{
		cache:              c,
		engine:             e,
		transactionManager: transaction.NewManager(),
		handlers:           make(map[string]command.Handler),
		specs:              make(map[string]apicommand.Spec),
		requirePass:        requirePass,
		blockingRegistry:   br,
		watchManager:       wm,
		tracker:            serverOps.NewTracker(),
	}
	b.registerAll()
	return b
}

// Register adds a single command handler and its argument spec.
func (b *Pipeline) Register(op string, reg command.Registration) {
	op = strings.ToUpper(op)
	b.handlers[op] = reg.Handler
	b.specs[op] = reg.Spec
}

// RegisterHandler adds a handler without a spec (for dynamic/test commands).
func (b *Pipeline) RegisterHandler(op string, handler command.Handler) {
	b.handlers[strings.ToUpper(op)] = handler
}

// The Set* methods below configure evaluator dependencies. They are NOT
// safe for concurrent use — all must be called during server startup,
// before any client connection is accepted.

func (b *Pipeline) SetPluginRouter(r *router.Router) {
	b.pluginRouter = r
}

func (b *Pipeline) SetHookExecutor(e apicommand.HookExecutor) {
	b.hookExecutor = e
}

func (b *Pipeline) SetEmitter(e events.Emitter) {
	b.emitter = e
}

func (b *Pipeline) SetTracker(t *serverOps.Tracker) {
	b.tracker = t
}

// SetPersistenceFeed wires the persistence coordinator's mutation-feed
// hook into the evaluator. Each *command.Context populated by fillCmdCtx
// inherits the same instance, so command.Dispatch can decide per-command
// whether to wrap the write closure with emission. Pass nil to disable
// emission (the default — no coordinator means no mutation feed).
func (b *Pipeline) SetPersistenceFeed(f command.MutationEmitter) {
	b.persistenceFeed = f
}

func (b *Pipeline) SetOpHookExecutor(e OpHookExecutor) {
	b.opHookExecutor = e
}

func (b *Pipeline) SetCommandMetricsRecorder(r CommandMetricsRecorder) {
	b.commandMetrics = r
}

// RegisterEmbeddedCommand registers a command provided by an embedded
// plugin (e.g. persistence plugins). The handler function is wrapped in
// a command.Handler adapter that routes through command.Dispatch for
// proper shard locking and mutation emission.
func (b *Pipeline) RegisterEmbeddedCommand(name string, fn func(context.Context, []string) (any, error), spec apicommand.Spec) {
	handler := func(cmdCtx *command.Context) apicommand.Result {
		executeFn := func() any {
			val, err := fn(cmdCtx.Context(), cmdCtx.Args)
			if err != nil {
				return err
			}
			return val
		}
		return command.Dispatch(cmdCtx, executeFn)
	}
	b.Register(name, command.Registration{Handler: handler, Spec: spec})
}

func (b *Pipeline) CoreCommandNames() []string {
	names := make([]string, 0, len(b.handlers))
	for name := range b.handlers {
		names = append(names, name)
	}
	return names
}

func (b *Pipeline) registerAll() {
	// RESP command handlers provide their own specs.
	for name, reg := range resphandler.Registrations() {
		b.Register(name, reg)
	}
	// REX command handlers provide their own specs.
	for name, reg := range rexhandler.Registrations() {
		b.Register(name, reg)
	}
}

func (b *Pipeline) Evaluate(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string) apicommand.Result {
	return b.evaluateCore(parentCtx, client, op, args, false, false)
}

// EvaluatePreLocked evaluates a command with ShardLocked=true, indicating
// the target shard's lock is already held by the caller. Used by pipeline
// batch coalescing where shard locks are pre-acquired.
func (b *Pipeline) EvaluatePreLocked(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string) apicommand.Result {
	return b.evaluateCore(parentCtx, client, op, args, false, true)
}

// SpecFor returns the argument spec for the given command name, or false
// if the command is not a registered core command.
func (b *Pipeline) SpecFor(op string) (apicommand.Spec, bool) {
	spec, ok := b.specs[strings.ToUpper(op)]
	return spec, ok
}

// evaluateInternal is the EvalFn-compatible entry point used by EXEC to
// re-enter the pipeline for queued commands. Delegates to evaluateCore
// with shardLocked=false.
func (b *Pipeline) evaluateInternal(parentCtx context.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool) apicommand.Result {
	return b.evaluateCore(parentCtx, ctx, op, args, inBatch, false)
}

func (b *Pipeline) evaluateCore(parentCtx context.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool, shardLocked bool) apicommand.Result {
	op = strings.ToUpper(op)

	handler, ok := b.handlers[op]
	if !ok {
		// Fall through to plugin router for plugin-provided commands.
		if b.pluginRouter != nil && b.pluginRouter.HasCommand(op) {
			return b.routeToPlugin(parentCtx, ctx, op, args)
		}
		logger.DebugNoCtx().Str("command", op).Msg("unknown command")
		return apicommand.Result{Value: resp.ErrUnknown(strings.ToLower(op))}
	}
	benchstats.RecordPipelineEvaluation()

	spec, hasSpec := b.specs[op]
	if hasSpec {
		n := len(args)
		if n < spec.Min || (spec.Max >= 0 && n > spec.Max) {
			return apicommand.Result{Value: resp.ErrArgs(strings.ToLower(op))}
		}
	}

	// Transactional logic: queue commands if in transaction, except for
	// transaction control commands and REX.META (connection state, like AUTH).
	if ctx.InTransaction && !inBatch {
		if op != resp.CmdMulti && op != resp.CmdExec && op != resp.CmdDiscard &&
			op != resp.CmdHello && op != resp.CmdRexMeta {
			if op == "QUIT" {
				return apicommand.Result{Value: "OK"}
			}
			ctx.EnqueueCommand(append([]string{op}, args...))
			return apicommand.Result{Value: "QUEUED"}
		}
	}

	// Note: Spec.ReadOnly is populated for all read-only commands but not
	// acted on here. An earlier iteration of #28 routed read-only commands
	// inline under cache.RLock() to skip the engine queue. Profile evidence
	// (diagnosis Finding 4) showed the engine channel hop was 42% of
	// pipelined-GET wait time, but the bypass branch introduced
	// sync.RWMutex mode-switching cost that regressed pipelined writes by
	// roughly the same amount it gained on reads. The classification stays
	// documented for future per-shard / lock-free designs that decouple
	// reader and writer paths properly. See the #28 PR body for the
	// measurement detail.

	// --- Fast path: no observers attached, skip the entire instrumentation
	// block. Profile attribution: bus.Emit + tracker.Start + tracker.Complete
	// account for ~80% of mutex-contention time on simple writes, plus 4×
	// ContextSnapshot allocations and 7× Enrich calls per command. None of
	// this work has a consumer when no plugin is wired, so we bypass it.
	//
	// The handler still receives a real *ops.Operation in opCtx so logger
	// correlation works; the operation is simply never registered, enriched,
	// emitted, or hook-fired.
	if !b.hasAnySink(op) {
		if b.hasCommandMetricsSink() {
			benchstats.RecordPipelineMetricsOnlyPath()
			return b.evaluateMetricsOnly(parentCtx, ctx, op, args, inBatch, handler, spec, shardLocked)
		}
		benchstats.RecordPipelineFastPath()
		return b.evaluateFast(parentCtx, ctx, op, args, inBatch, handler, spec, shardLocked)
	}
	benchstats.RecordPipelineFullPath()

	// --- Create command operation ---
	cmdOp := b.tracker.Start(ops.TypeCommand, ctx.OperationID)
	startNs := cmdOp.StartTime.UnixNano()

	// Inject server context into operation.
	cmdOp.Enrich(apicommand.StartNs, strconv.FormatInt(startNs, 10))
	cmdOp.Enrich(apicommand.OperationID, cmdOp.ID)
	cmdOp.Enrich(apicommand.CommandKey, op)
	cmdOp.Enrich(apicommand.ArgCountKey, strconv.Itoa(len(args)))
	if hasSpec && spec.ReadOnly {
		cmdOp.Enrich(apicommand.ReadOnlyKey, "true")
	}

	// Inject REX metadata into operation context.
	if ctx.RexMeta != nil || len(ctx.CmdMeta) > 0 {
		metadata := rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)
		for k, v := range metadata {
			cmdOp.Enrich(rex.Prefix+k, v)
		}
	}

	// Build operation-carrying context so handlers and downstream (cache,
	// persistence) can log with correlation. Derives from the parent
	// (connection) context so cancellation propagates into plugin routing.
	opCtx := ops.WithContext(parentCtx, cmdOp)

	// Fire operation start hooks (synchronous — enriches context before work).
	if b.hasCommandOperationHookSink() {
		b.opHookExecutor.RunStartHooks(opCtx, cmdOp)
	}

	// Emit operation.started + command.started events only for interested subscribers.
	if b.emitter != nil {
		if b.emitter.HasSubscribersFor(events.OperationStarted) {
			buildStart := benchstats.StartTimer()
			snapshotStart := benchstats.StartTimer()
			snapshot := cmdOp.ContextSnapshot(false)
			benchstats.RecordPipelineContextSnapshot(snapshotStart)
			evt := events.NewOperationStarted(cmdOp.ID, string(cmdOp.Type), cmdOp.ParentID, snapshot)
			benchstats.RecordPipelineOperationStartedBuilt(buildStart)
			b.emitter.Emit(evt)
		}
		if b.emitter.HasSubscribersFor(events.CommandStarted) {
			buildStart := benchstats.StartTimer()
			evt := events.NewCommandStarted(op, args, rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)).WithOperationID(cmdOp.ID)
			benchstats.RecordPipelineCommandStartedBuilt(buildStart)
			b.emitter.Emit(evt)
		}
	}

	cmdCtx := cmdCtxPool.Get().(*command.Context)
	defer putCmdCtx(cmdCtx)
	b.fillCmdCtx(cmdCtx, ctx, op, args, inBatch, spec)
	cmdCtx.ShardLocked = shardLocked
	cmdCtx.SetContext(opCtx)

	// --- Command hooks (pre) ---
	var hookCtx map[string]string
	hasHooks := b.hasCommandHookSink(op)
	connInfo := &gcpc.ConnectionInfoV1{Id: ctx.ConnectionID, RemoteAddr: ctx.RemoteAddr}
	cmdInfo := &gcpc.CommandInfoV1{Name: op, Args: args}
	if hasHooks {
		snapshotStart := benchstats.StartTimer()
		hookCtx = cmdOp.ContextSnapshot(false)
		benchstats.RecordPipelineContextSnapshot(snapshotStart)

		if pre := b.hookExecutor.RunPreHooks(opCtx, cmdInfo, connInfo, hookCtx); pre != nil {
			if pre.Denied {
				cmdOp.Fail("denied: " + pre.DenyReason)
				if b.opHookExecutor != nil {
					b.opHookExecutor.RunCompleteHooks(cmdOp)
				}
				b.tracker.Fail(cmdOp.ID, "denied: "+pre.DenyReason)
				return apicommand.Result{Value: resp.MarshalError("DENIED " + pre.DenyReason)}
			}
			hookCtx = pre.Context
			for k, v := range hookCtx {
				cmdOp.Enrich(k, v)
			}
		}
	}

	// --- Execute command handler ---
	result := handler(cmdCtx)

	// --- Command hooks (post) ---
	if hasHooks && hookCtx != nil {
		elapsedNs := time.Now().UnixNano() - startNs
		hookCtx[apicommand.ElapsedNs] = strconv.FormatInt(elapsedNs, 10)
		resultVal, resultErr := resultToHookStrings(result)
		b.hookExecutor.RunPostHooks(opCtx, cmdInfo, connInfo, resultVal, resultErr, hookCtx)
	}

	// --- Complete operation ---
	cmdOp.Complete()
	elapsedNs := uint64(cmdOp.Duration().Nanoseconds())
	resultVal, resultErr := resultToHookStrings(result)

	cmdOp.Enrich(apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10))
	cmdOp.Enrich(apicommand.ResultKey, resultVal)
	if resultErr != "" {
		cmdOp.Enrich(apicommand.ErrorKey, resultErr)
	}
	b.recordCommandMetric(op, elapsedNs, resultErr != "")

	if b.hasCommandOperationHookSink() {
		b.opHookExecutor.RunCompleteHooks(cmdOp)
	}

	if b.emitter != nil {
		if b.emitter.HasSubscribersFor(events.CommandCompleted) {
			buildStart := benchstats.StartTimer()
			evt := events.NewCommandCompleted(op, args, elapsedNs, resultVal, resultErr, rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)).WithOperationID(cmdOp.ID)
			benchstats.RecordPipelineCommandCompletedBuilt(buildStart)
			b.emitter.Emit(evt)
		}
		if b.emitter.HasSubscribersFor(events.OperationCompleted) {
			buildStart := benchstats.StartTimer()
			snapshotStart := benchstats.StartTimer()
			snapshot := cmdOp.ContextSnapshot(false)
			benchstats.RecordPipelineContextSnapshot(snapshotStart)
			evt := events.NewOperationCompleted(cmdOp.ID, string(cmdOp.Type), elapsedNs, "completed", "", snapshot)
			benchstats.RecordPipelineOperationCompletedBuilt(buildStart)
			b.emitter.Emit(evt)
		}
	}

	b.tracker.Complete(cmdOp.ID)
	return result
}

// evaluateFast runs the handler with a bare *ops.Operation and no
// instrumentation. Hot path when no sinks are attached. See hasAnySink for
// the documented invariant on late-subscriber visibility.
func (b *Pipeline) evaluateFast(parentCtx context.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool, handler command.Handler, spec apicommand.Spec, shardLocked bool) apicommand.Result {
	cmdOp := ops.New(ops.TypeCommand, ctx.OperationID)
	opCtx := ops.WithContext(parentCtx, cmdOp)

	cmdCtx := cmdCtxPool.Get().(*command.Context)
	defer putCmdCtx(cmdCtx)
	b.fillCmdCtx(cmdCtx, ctx, op, args, inBatch, spec)
	cmdCtx.ShardLocked = shardLocked
	cmdCtx.SetContext(opCtx)

	result := handler(cmdCtx)

	if b.tracker != nil {
		b.tracker.IncrementSkipped()
	}
	return result
}

// evaluateMetricsOnly keeps Prometheus-style metrics off the full operation,
// event, and hook path. It measures the handler directly, records the compact
// tuple, and still skips tracker registration/enrichment just like evaluateFast.
func (b *Pipeline) evaluateMetricsOnly(parentCtx context.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool, handler command.Handler, spec apicommand.Spec, shardLocked bool) apicommand.Result {
	cmdOp := ops.New(ops.TypeCommand, ctx.OperationID)
	opCtx := ops.WithContext(parentCtx, cmdOp)

	cmdCtx := cmdCtxPool.Get().(*command.Context)
	defer putCmdCtx(cmdCtx)
	b.fillCmdCtx(cmdCtx, ctx, op, args, inBatch, spec)
	cmdCtx.ShardLocked = shardLocked
	cmdCtx.SetContext(opCtx)

	result := handler(cmdCtx)
	elapsedNs := uint64(time.Since(cmdOp.StartTime).Nanoseconds())
	b.recordCommandMetric(op, elapsedNs, resultHasError(result))

	if b.tracker != nil {
		b.tracker.IncrementSkipped()
	}
	return result
}

func (b *Pipeline) recordCommandMetric(op string, elapsedNs uint64, isError bool) {
	if b.commandMetrics != nil {
		b.commandMetrics.RecordCommand(op, elapsedNs, isError)
	}
}

func resultHasError(r apicommand.Result) bool {
	return r.Err != nil
}

// fillCmdCtx populates a *command.Context for one command. Centralises the
// field assignment so the fast path and the slow path share the exact set
// of dependencies — drift between the two would surface as a nil-pointer
// crash inside the handler under one path but not the other.
//
// Computes the sharding routing fields (Shard / MultiKey) from the
// command's Spec so command.Dispatch can decide whether to take a single
// shard's lock (single-key), all shard locks (multi-key), or run inline
// (keyless) — see command.Dispatch for the routing matrix.
func (b *Pipeline) fillCmdCtx(c *command.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool, spec apicommand.Spec) {
	c.Client = ctx
	c.Op = op
	c.Args = args
	c.InBatch = inBatch
	c.Engine = b.engine
	c.Cache = b.cache
	c.Transaction = b.transactionManager
	c.BlockingRegistry = b.blockingRegistry
	c.WatchManager = b.watchManager
	c.RequirePass = b.requirePass
	c.MultiKey = spec.MultiKey
	switch {
	case spec.MultiKey:
		c.Shard = -1 // ignored when MultiKey is true; set defensively
	case spec.KeyArgIndex < 0:
		c.Shard = -1 // keyless
	case spec.KeyArgIndex < len(args):
		c.Shard = b.cache.ShardIndexOf(args[spec.KeyArgIndex])
	default:
		// Spec.KeyArgIndex points past Args — register_test.go guarantees
		// this is unreachable in practice (KeyArgIndex < Min). Fall back to
		// shard 0 so behaviour is deterministic.
		c.Shard = 0
	}
	c.EvalFn = b.evaluateInternal
	c.Spec = spec
	c.Coordinator = b.persistenceFeed
}

// routeToPlugin dispatches a command to a plugin via the router. The per-call
// timeout is derived from parentCtx so connection-level cancellation also
// aborts the plugin call.
func (b *Pipeline) routeToPlugin(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string) apicommand.Result {
	metadata := rex.BuildMetadata(client.RexMeta, client.CmdMeta)
	connInfo := &gcpc.ConnectionInfoV1{Id: client.ConnectionID, RemoteAddr: client.RemoteAddr}

	ctx, cancel := context.WithTimeout(parentCtx, pluginCommandTimeout)
	defer cancel()

	if b.hasAnySink(op) {
		cmdOp := b.tracker.Start(ops.TypeCommand, client.OperationID)
		cmdOp.Enrich(apicommand.CommandKey, op)
		cmdOp.Enrich(apicommand.ArgCountKey, strconv.Itoa(len(args)))
		if meta := b.pluginRouter.LookupMeta(op); meta != nil && meta.ReadOnly {
			cmdOp.Enrich(apicommand.ReadOnlyKey, "true")
		}
		ctx = ops.WithContext(ctx, cmdOp)
		defer func() { cmdOp.Complete(); b.tracker.Complete(cmdOp.ID) }()
	}

	val, suppress, err := b.pluginRouter.Route(ctx, op, args, metadata, connInfo)
	if err != nil {
		if errors.Is(err, router.ErrPluginTimeout) {
			return apicommand.Result{Value: resp.MarshalError("ERR plugin timeout")}
		}
		if errors.Is(err, router.ErrPluginDown) {
			return apicommand.Result{Value: resp.MarshalError("ERR plugin unavailable")}
		}
		return apicommand.Result{Err: err}
	}
	if e, ok := val.(error); ok {
		return apicommand.Result{Err: e}
	}
	return apicommand.Result{Value: val, SuppressResponse: suppress}
}

func resultToHookStrings(r apicommand.Result) (string, string) {
	if r.Err != nil {
		return "", r.Err.Error()
	}
	if r.Value == nil {
		return "", ""
	}
	return fmt.Sprintf("%v", r.Value), ""
}
