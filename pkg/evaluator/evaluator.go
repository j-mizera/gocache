package evaluator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gocache/api/events"
	"gocache/api/logger"
	ops "gocache/api/operations"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/plugin/router"
	"gocache/pkg/resp"
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
	RunStartHooks(ctx context.Context, op *ops.Operation)
	RunCompleteHooks(op *ops.Operation)
}

// BaseEvaluator is the command dispatch pipeline. It owns no command-specific
// knowledge — handlers and their argument specs are provided by external
// packages (resp/handler, rex/handler) via command.Registration.
//
// Following "accept interfaces, return structs," constructors return
// *BaseEvaluator directly; consumers that need a narrower surface for testing
// should define their own interface locally (pkg/server does this via a
// package-private alias of the methods it actually calls).
type BaseEvaluator struct {
	cache              *cache.Cache
	engine             *engine.Engine
	transactionManager *transaction.Manager
	handlers           map[string]command.Handler
	specs              map[string]command.Spec
	requirePass        string
	blockingRegistry   *blocking.Registry
	watchManager       *watch.Manager
	pluginRouter       *router.Router
	hookExecutor       command.HookExecutor
	emitter            events.Emitter
	tracker            *serverOps.Tracker
	opHookExecutor     OpHookExecutor
	persistenceFeed    command.MutationEmitter
	snapshotInvoker    command.SnapshotInvoker
}

func New(c *cache.Cache, e *engine.Engine, requirePass string, br *blocking.Registry, wm *watch.Manager) *BaseEvaluator {
	b := &BaseEvaluator{
		cache:              c,
		engine:             e,
		transactionManager: transaction.NewManager(),
		handlers:           make(map[string]command.Handler),
		specs:              make(map[string]command.Spec),
		requirePass:        requirePass,
		blockingRegistry:   br,
		watchManager:       wm,
		tracker:            serverOps.NewTracker(),
	}
	b.registerAll()
	return b
}

// Register adds a single command handler and its argument spec.
func (b *BaseEvaluator) Register(op string, reg command.Registration) {
	op = strings.ToUpper(op)
	b.handlers[op] = reg.Handler
	b.specs[op] = reg.Spec
}

// RegisterHandler adds a handler without a spec (for dynamic/test commands).
func (b *BaseEvaluator) RegisterHandler(op string, handler command.Handler) {
	b.handlers[strings.ToUpper(op)] = handler
}

func (b *BaseEvaluator) SetPluginRouter(r *router.Router) {
	b.pluginRouter = r
}

func (b *BaseEvaluator) SetHookExecutor(e command.HookExecutor) {
	b.hookExecutor = e
}

func (b *BaseEvaluator) SetEmitter(e events.Emitter) {
	b.emitter = e
}

func (b *BaseEvaluator) SetTracker(t *serverOps.Tracker) {
	b.tracker = t
}

// SetPersistenceFeed wires the persistence coordinator's mutation-feed
// hook into the evaluator. Each *command.Context populated by fillCmdCtx
// inherits the same instance, so command.Dispatch can decide per-command
// whether to wrap the write closure with emission. Pass nil to disable
// emission (the default — no coordinator means no mutation feed).
func (b *BaseEvaluator) SetPersistenceFeed(f command.MutationEmitter) {
	b.persistenceFeed = f
}

// SetSnapshotInvoker wires the persistence coordinator's SAVE/BGSAVE
// entry point into the evaluator. fillCmdCtx threads it onto each
// *command.Context so the snapshot handler doesn't have to import
// pkg/persistence directly. Pass nil to disable snapshot commands —
// the handler returns an error to the client when invoked without a
// registered snapshotter.
func (b *BaseEvaluator) SetSnapshotInvoker(s command.SnapshotInvoker) {
	b.snapshotInvoker = s
}

func (b *BaseEvaluator) SetOpHookExecutor(e OpHookExecutor) {
	b.opHookExecutor = e
}

func (b *BaseEvaluator) CoreCommandNames() []string {
	names := make([]string, 0, len(b.handlers))
	for name := range b.handlers {
		names = append(names, name)
	}
	return names
}

func (b *BaseEvaluator) registerAll() {
	// RESP command handlers provide their own specs.
	for name, reg := range resphandler.Registrations() {
		b.Register(name, reg)
	}
	// REX command handlers provide their own specs.
	for name, reg := range rexhandler.Registrations() {
		b.Register(name, reg)
	}
}

func (b *BaseEvaluator) Evaluate(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string) command.Result {
	return b.evaluateInternal(parentCtx, client, op, args, false)
}

func (b *BaseEvaluator) evaluateInternal(parentCtx context.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool) command.Result {
	op = strings.ToUpper(op)

	handler, ok := b.handlers[op]
	if !ok {
		// Fall through to plugin router for plugin-provided commands.
		if b.pluginRouter != nil && b.pluginRouter.HasCommand(op) {
			return b.routeToPlugin(parentCtx, ctx, op, args)
		}
		logger.DebugNoCtx().Str("command", op).Msg("unknown command")
		return command.Result{Value: resp.ErrUnknown(strings.ToLower(op))}
	}

	spec, hasSpec := b.specs[op]
	if hasSpec {
		n := len(args)
		if n < spec.Min || (spec.Max >= 0 && n > spec.Max) {
			return command.Result{Value: resp.ErrArgs(strings.ToLower(op))}
		}
	}

	// Transactional logic: queue commands if in transaction, except for
	// transaction control commands and REX.META (connection state, like AUTH).
	if ctx.InTransaction && !inBatch {
		if op != resp.CmdMulti && op != resp.CmdExec && op != resp.CmdDiscard &&
			op != resp.CmdHello && op != resp.CmdRexMeta {
			if op == "QUIT" {
				return command.Result{Value: "OK"}
			}
			ctx.EnqueueCommand(append([]string{op}, args...))
			return command.Result{Value: "QUEUED"}
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
	if !b.hasAnySink() {
		return b.evaluateFast(parentCtx, ctx, op, args, inBatch, handler, spec)
	}

	// --- Create command operation ---
	cmdOp := b.tracker.Start(ops.TypeCommand, ctx.OperationID)
	startNs := cmdOp.StartTime.UnixNano()

	// Inject server context into operation.
	cmdOp.Enrich(command.StartNs, strconv.FormatInt(startNs, 10))
	cmdOp.Enrich(command.OperationID, cmdOp.ID)
	cmdOp.Enrich(command.CommandKey, op)
	cmdOp.Enrich(command.ArgCountKey, strconv.Itoa(len(args)))

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
	if b.opHookExecutor != nil && b.opHookExecutor.HasAny() {
		b.opHookExecutor.RunStartHooks(opCtx, cmdOp)
	}

	// Emit operation.start + command.pre events.
	if b.emitter != nil {
		b.emitter.Emit(events.NewOperationStart(cmdOp.ID, string(cmdOp.Type), cmdOp.ParentID, cmdOp.ContextSnapshot(false)))
		b.emitter.Emit(events.NewCommandPre(op, args, rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)).WithOperationID(cmdOp.ID))
	}

	cmdCtx := cmdCtxPool.Get().(*command.Context)
	defer putCmdCtx(cmdCtx)
	b.fillCmdCtx(cmdCtx, ctx, op, args, inBatch, spec)
	cmdCtx.SetContext(opCtx)

	// --- Command hooks (pre) ---
	var hookCtx map[string]string
	hasHooks := b.hookExecutor != nil && b.hookExecutor.HasAny()
	if hasHooks {
		hookCtx = cmdOp.ContextSnapshot(false)

		if pre := b.hookExecutor.RunPreHooks(opCtx, op, args, hookCtx); pre != nil {
			if pre.Denied {
				cmdOp.Fail("denied: " + pre.DenyReason)
				if b.opHookExecutor != nil {
					b.opHookExecutor.RunCompleteHooks(cmdOp)
				}
				b.tracker.Fail(cmdOp.ID, "denied: "+pre.DenyReason)
				return command.Result{Value: resp.MarshalError("DENIED " + pre.DenyReason)}
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
		hookCtx[command.ElapsedNs] = strconv.FormatInt(elapsedNs, 10)
		resultVal, resultErr := resultToHookStrings(result)
		b.hookExecutor.RunPostHooks(opCtx, op, args, resultVal, resultErr, hookCtx)
	}

	// --- Complete operation ---
	cmdOp.Complete()
	elapsedNs := uint64(cmdOp.Duration().Nanoseconds())
	resultVal, resultErr := resultToHookStrings(result)

	cmdOp.Enrich(command.ElapsedNs, strconv.FormatUint(elapsedNs, 10))
	cmdOp.Enrich(command.ResultKey, resultVal)
	if resultErr != "" {
		cmdOp.Enrich(command.ErrorKey, resultErr)
	}

	if b.opHookExecutor != nil && b.opHookExecutor.HasAny() {
		b.opHookExecutor.RunCompleteHooks(cmdOp)
	}

	if b.emitter != nil {
		b.emitter.Emit(events.NewCommandPost(op, args, elapsedNs, resultVal, resultErr, rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)).WithOperationID(cmdOp.ID))
		b.emitter.Emit(events.NewOperationComplete(cmdOp.ID, string(cmdOp.Type), elapsedNs, "completed", "", cmdOp.ContextSnapshot(false)))
	}

	b.tracker.Complete(cmdOp.ID)
	return result
}

// evaluateFast runs the handler with a bare *ops.Operation and no
// instrumentation. Hot path when no sinks are attached. See hasAnySink for
// the documented invariant on late-subscriber visibility.
func (b *BaseEvaluator) evaluateFast(parentCtx context.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool, handler command.Handler, spec command.Spec) command.Result {
	cmdOp := ops.New(ops.TypeCommand, ctx.OperationID)
	opCtx := ops.WithContext(parentCtx, cmdOp)

	cmdCtx := cmdCtxPool.Get().(*command.Context)
	defer putCmdCtx(cmdCtx)
	b.fillCmdCtx(cmdCtx, ctx, op, args, inBatch, spec)
	cmdCtx.SetContext(opCtx)

	result := handler(cmdCtx)

	if b.tracker != nil {
		b.tracker.IncrementSkipped()
	}
	return result
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
func (b *BaseEvaluator) fillCmdCtx(c *command.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool, spec command.Spec) {
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
	c.Snapshotter = b.snapshotInvoker
}

// routeToPlugin dispatches a command to a plugin via the router. The per-call
// timeout is derived from parentCtx so connection-level cancellation also
// aborts the plugin call.
func (b *BaseEvaluator) routeToPlugin(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string) command.Result {
	metadata := rex.BuildMetadata(client.RexMeta, client.CmdMeta)

	ctx, cancel := context.WithTimeout(parentCtx, pluginCommandTimeout)
	defer cancel()

	val, err := b.pluginRouter.Route(ctx, op, args, metadata)
	if err != nil {
		// Known sentinels produce stable wire messages; the mapToResp pipeline
		// formats the default "ERR <msg>" for anything else via Result.Err.
		if errors.Is(err, router.ErrPluginTimeout) {
			return command.Result{Value: resp.MarshalError("ERR plugin timeout")}
		}
		if errors.Is(err, router.ErrPluginDown) {
			return command.Result{Value: resp.MarshalError("ERR plugin unavailable")}
		}
		return command.Result{Err: err}
	}
	if e, ok := val.(error); ok {
		return command.Result{Err: e}
	}
	return command.Result{Value: val}
}

func resultToHookStrings(r command.Result) (string, string) {
	if r.Err != nil {
		return "", r.Err.Error()
	}
	if r.Value == nil {
		return "", ""
	}
	return fmt.Sprintf("%v", r.Value), ""
}
