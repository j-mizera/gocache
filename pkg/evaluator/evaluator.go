package evaluator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gocache/api/events"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/logger"
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

// Evaluator is the command dispatch pipeline.
type Evaluator interface {
	Evaluate(ctx *clientctx.ClientContext, op string, args []string) command.Result
	RegisterHandler(op string, handler command.Handler)
	SetPluginRouter(r *router.Router)
	SetHookExecutor(e command.HookExecutor)
	SetEmitter(e events.Emitter)
	CoreCommandNames() []string
}

// BaseEvaluator is the pipeline implementation. It owns no command-specific
// knowledge — handlers and their argument specs are provided by external
// packages (resp/handler, rex/handler) via command.Registration.
type BaseEvaluator struct {
	cache              *cache.Cache
	engine             *engine.Engine
	transactionManager *transaction.Manager
	handlers           map[string]command.Handler
	specs              map[string]command.Spec
	snapshotFile       string
	requirePass        string
	blockingRegistry   *blocking.Registry
	watchManager       *watch.Manager
	pluginRouter       *router.Router
	hookExecutor       command.HookExecutor
	emitter            events.Emitter
}

func New(c *cache.Cache, e *engine.Engine, snapshotFile, requirePass string, br *blocking.Registry, wm *watch.Manager) Evaluator {
	b := &BaseEvaluator{
		cache:              c,
		engine:             e,
		transactionManager: transaction.NewManager(),
		handlers:           make(map[string]command.Handler),
		specs:              make(map[string]command.Spec),
		snapshotFile:       snapshotFile,
		requirePass:        requirePass,
		blockingRegistry:   br,
		watchManager:       wm,
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

func (b *BaseEvaluator) Evaluate(ctx *clientctx.ClientContext, op string, args []string) command.Result {
	return b.evaluateInternal(ctx, op, args, false)
}

func (b *BaseEvaluator) evaluateInternal(ctx *clientctx.ClientContext, op string, args []string, inBatch bool) command.Result {
	op = strings.ToUpper(op)

	handler, ok := b.handlers[op]
	if !ok {
		// Fall through to plugin router for plugin-provided commands.
		if b.pluginRouter != nil && b.pluginRouter.HasCommand(op) {
			return b.routeToPlugin(ctx, op, args)
		}
		logger.Debug().Str("command", op).Msg("unknown command")
		return command.Result{Value: resp.ErrUnknown(strings.ToLower(op))}
	}

	if spec, hasSpec := b.specs[op]; hasSpec {
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

	// Emit command.pre event.
	if b.emitter != nil {
		b.emitter.Emit(events.NewCommandPre(op, args, rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)))
	}

	cmdCtx := &command.Context{
		Client:           ctx,
		Op:               op,
		Args:             args,
		InBatch:          inBatch,
		Engine:           b.engine,
		Cache:            b.cache,
		Transaction:      b.transactionManager,
		BlockingRegistry: b.blockingRegistry,
		WatchManager:     b.watchManager,
		SnapshotFile:     b.snapshotFile,
		RequirePass:      b.requirePass,
		EvalFn:           b.evaluateInternal,
	}

	// Build hook context with server-injected values.
	var (
		hookCtx map[string]string
		startNs int64
	)
	if b.hookExecutor != nil && b.hookExecutor.HasAny() {
		startNs = time.Now().UnixNano()
		hookCtx = command.NewHookCtx()
		hookCtx[command.StartNs] = strconv.FormatInt(startNs, 10)

		// Inject REX metadata into hook context (connection defaults + per-command).
		if ctx.RexMeta != nil || len(ctx.CmdMeta) > 0 {
			rex.InjectIntoHookCtx(hookCtx, ctx.RexMeta, ctx.CmdMeta)
		}

		// Pre-hooks: fire before command execution.
		// NOTE: context.Background() is used because Evaluate does not yet thread
		// a per-request context through its signature. See roadmap for the
		// context-propagation refactor.
		if pre := b.hookExecutor.RunPreHooks(context.Background(), op, args, hookCtx); pre != nil {
			if pre.Denied {
				return command.Result{Value: resp.MarshalError("DENIED " + pre.DenyReason)}
			}
			hookCtx = pre.Context
		}
	}

	result := handler(cmdCtx)

	// Post-hooks: fire after command execution.
	if b.hookExecutor != nil && b.hookExecutor.HasAny() && hookCtx != nil {
		// Use the locally captured startNs -- not a round-trip through the map.
		// A plugin could have deleted _start_ns from hookCtx during pre-hooks,
		// and parsing back out of the map would silently yield 0 elapsed.
		elapsedNs := time.Now().UnixNano() - startNs
		hookCtx[command.ElapsedNs] = strconv.FormatInt(elapsedNs, 10)
		resultVal, resultErr := resultToHookStrings(result)
		b.hookExecutor.RunPostHooks(context.Background(), op, args, resultVal, resultErr, hookCtx)
	}

	// Emit command.post event.
	if b.emitter != nil {
		var elapsedNs uint64
		if startNs > 0 {
			elapsedNs = uint64(time.Now().UnixNano() - startNs)
		}
		resultVal, resultErr := resultToHookStrings(result)
		b.emitter.Emit(events.NewCommandPost(op, args, elapsedNs, resultVal, resultErr, rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)))
	}

	return result
}

// routeToPlugin dispatches a command to a plugin via the router.
func (b *BaseEvaluator) routeToPlugin(client *clientctx.ClientContext, op string, args []string) command.Result {
	metadata := rex.BuildMetadata(client.RexMeta, client.CmdMeta)

	ctx, cancel := context.WithTimeout(context.Background(), pluginCommandTimeout)
	defer cancel()

	val, err := b.pluginRouter.Route(ctx, op, args, metadata)
	if err != nil {
		if errors.Is(err, router.ErrPluginTimeout) {
			return command.Result{Value: resp.MarshalError("ERR plugin timeout")}
		}
		if errors.Is(err, router.ErrPluginDown) {
			return command.Result{Value: resp.MarshalError("ERR plugin unavailable")}
		}
		return command.Result{Value: resp.MarshalError("ERR " + err.Error())}
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
