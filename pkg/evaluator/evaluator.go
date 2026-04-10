package evaluator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/logger"
	"gocache/pkg/plugin/hooks"
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

// commandSpecs defines argument count constraints for each command.
var commandSpecs = map[string]command.Spec{
	resp.CmdSet:          {Min: 2, Max: -1},
	resp.CmdGet:          {Min: 1, Max: 1},
	resp.CmdDelete:       {Min: 1, Max: -1},
	resp.CmdExists:       {Min: 1, Max: 1},
	resp.CmdExpire:       {Min: 2, Max: 2},
	resp.CmdTTL:          {Min: 1, Max: 1},
	resp.CmdLPush:        {Min: 2, Max: -1},
	resp.CmdRPush:        {Min: 2, Max: -1},
	resp.CmdLPop:         {Min: 1, Max: 1},
	resp.CmdRPop:         {Min: 1, Max: 1},
	resp.CmdLLen:         {Min: 1, Max: 1},
	resp.CmdLRange:       {Min: 3, Max: 3},
	resp.CmdBLPop:        {Min: 2, Max: -1},
	resp.CmdBRPop:        {Min: 2, Max: -1},
	resp.CmdHSet:         {Min: 3, Max: -1},
	resp.CmdHGet:         {Min: 2, Max: 2},
	resp.CmdHDel:         {Min: 2, Max: -1},
	resp.CmdHExists:      {Min: 2, Max: 2},
	resp.CmdHGetAll:      {Min: 1, Max: 1},
	resp.CmdHKeys:        {Min: 1, Max: 1},
	resp.CmdHVals:        {Min: 1, Max: 1},
	resp.CmdHLen:         {Min: 1, Max: 1},
	resp.CmdSAdd:         {Min: 2, Max: -1},
	resp.CmdSRem:         {Min: 2, Max: -1},
	resp.CmdSMembers:     {Min: 1, Max: 1},
	resp.CmdSIsMember:    {Min: 2, Max: 2},
	resp.CmdSCard:        {Min: 1, Max: 1},
	resp.CmdSPop:         {Min: 1, Max: 1},
	resp.CmdZAdd:         {Min: 3, Max: -1},
	resp.CmdZRem:         {Min: 2, Max: -1},
	resp.CmdZScore:       {Min: 2, Max: 2},
	resp.CmdZCard:        {Min: 1, Max: 1},
	resp.CmdZRange:       {Min: 3, Max: 4},
	resp.CmdZRank:        {Min: 2, Max: 2},
	resp.CmdZCount:       {Min: 3, Max: 3},
	resp.CmdMulti:        {Min: 0, Max: 0},
	resp.CmdExec:         {Min: 0, Max: 0},
	resp.CmdDiscard:      {Min: 0, Max: 0},
	resp.CmdSnapshot:     {Min: 0, Max: 0},
	resp.CmdLoadSnapshot: {Min: 1, Max: 1},
	resp.CmdDBSize:       {Min: 0, Max: 0},
	resp.CmdInfo:         {Min: 0, Max: 1},
	resp.CmdHello:        {Min: 1, Max: -1},

	// Server/connection commands
	resp.CmdPing:     {Min: 0, Max: 1},
	resp.CmdEcho:     {Min: 1, Max: 1},
	resp.CmdSelect:   {Min: 1, Max: 1},
	resp.CmdFlushDB:  {Min: 0, Max: 0},
	resp.CmdFlushAll: {Min: 0, Max: 0},
	resp.CmdAuth:     {Min: 1, Max: 1},

	// String counter commands
	resp.CmdIncr:        {Min: 1, Max: 1},
	resp.CmdDecr:        {Min: 1, Max: 1},
	resp.CmdIncrBy:      {Min: 2, Max: 2},
	resp.CmdDecrBy:      {Min: 2, Max: 2},
	resp.CmdIncrByFloat: {Min: 2, Max: 2},
	resp.CmdAppend:      {Min: 2, Max: 2},
	resp.CmdStrlen:      {Min: 1, Max: 1},

	// Multi-key commands
	resp.CmdMGet: {Min: 1, Max: -1},
	resp.CmdMSet: {Min: 2, Max: -1},

	// SET variants and TTL commands
	resp.CmdSetNX:   {Min: 2, Max: 2},
	resp.CmdPExpire: {Min: 2, Max: 2},
	resp.CmdPTTL:    {Min: 1, Max: 1},

	// Set operations
	resp.CmdSInter: {Min: 1, Max: -1},
	resp.CmdSUnion: {Min: 1, Max: -1},
	resp.CmdSDiff:  {Min: 1, Max: -1},

	// Key management commands
	resp.CmdType:      {Min: 1, Max: 1},
	resp.CmdRename:    {Min: 2, Max: 2},
	resp.CmdRenameNX:  {Min: 2, Max: 2},
	resp.CmdKeys:      {Min: 1, Max: 1},
	resp.CmdScan:      {Min: 1, Max: -1},
	resp.CmdRandomKey: {Min: 0, Max: 0},

	// Watch commands
	resp.CmdWatch:   {Min: 1, Max: -1},
	resp.CmdUnwatch: {Min: 0, Max: 0},

	// Key introspection
	resp.CmdObject: {Min: 1, Max: 2},

	// REX metadata
	resp.CmdRexMeta: {Min: 1, Max: -1},
}

// Evaluator is the command dispatch pipeline.
type Evaluator interface {
	Evaluate(ctx *clientctx.ClientContext, op string, args []string) command.Result
	RegisterHandler(op string, handler command.Handler)
	SetPluginRouter(r *router.Router)
	SetHookExecutor(e *hooks.Executor)
	CoreCommandNames() []string
}

// BaseEvaluator is the pipeline implementation.
type BaseEvaluator struct {
	cache              *cache.Cache
	engine             *engine.Engine
	transactionManager *transaction.Manager
	handlers           map[string]command.Handler
	snapshotFile       string
	requirePass        string
	blockingRegistry   *blocking.Registry
	watchManager       *watch.Manager
	pluginRouter       *router.Router
	hookExecutor       *hooks.Executor
}

func New(c *cache.Cache, e *engine.Engine, snapshotFile, requirePass string, br *blocking.Registry, wm *watch.Manager) Evaluator {
	b := &BaseEvaluator{
		cache:              c,
		engine:             e,
		transactionManager: transaction.NewManager(),
		handlers:           make(map[string]command.Handler),
		snapshotFile:       snapshotFile,
		requirePass:        requirePass,
		blockingRegistry:   br,
		watchManager:       wm,
	}
	b.registerHandlers()
	return b
}

func (b *BaseEvaluator) RegisterHandler(op string, handler command.Handler) {
	b.handlers[strings.ToUpper(op)] = handler
}

func (b *BaseEvaluator) SetPluginRouter(r *router.Router) {
	b.pluginRouter = r
}

func (b *BaseEvaluator) SetHookExecutor(e *hooks.Executor) {
	b.hookExecutor = e
}

func (b *BaseEvaluator) CoreCommandNames() []string {
	names := make([]string, 0, len(b.handlers))
	for name := range b.handlers {
		names = append(names, name)
	}
	return names
}

func (b *BaseEvaluator) registerHandlers() {
	// Register all RESP command handlers.
	for name, handler := range resphandler.Handlers() {
		b.handlers[name] = handler
	}
	// Register REX metadata handler.
	b.handlers[resp.CmdRexMeta] = rexhandler.HandleRexMeta
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
			return b.routeToPlugin(op, args)
		}
		logger.Debug().Str("command", op).Msg("unknown command")
		return command.Result{Value: resp.ErrUnknown(strings.ToLower(op))}
	}

	if spec, hasSpec := commandSpecs[op]; hasSpec {
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
	var hookCtx map[string]string
	if b.hookExecutor != nil && b.hookExecutor.HasAny() {
		startNs := time.Now().UnixNano()
		hookCtx = command.NewHookCtx()
		hookCtx[command.StartNs] = strconv.FormatInt(startNs, 10)

		// Inject REX metadata into hook context (connection defaults + per-command).
		if ctx.RexMeta != nil || len(ctx.CmdMeta) > 0 {
			rex.InjectIntoHookCtx(hookCtx, ctx.RexMeta, ctx.CmdMeta)
		}

		// Pre-hooks: fire before command execution.
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
		elapsedNs := time.Now().UnixNano() - mustParseInt64(hookCtx[command.StartNs])
		hookCtx[command.ElapsedNs] = strconv.FormatInt(elapsedNs, 10)
		resultVal, resultErr := resultToHookStrings(result)
		b.hookExecutor.RunPostHooks(context.Background(), op, args, resultVal, resultErr, hookCtx)
	}

	return result
}

// routeToPlugin dispatches a command to a plugin via the router.
func (b *BaseEvaluator) routeToPlugin(op string, args []string) command.Result {
	ctx, cancel := context.WithTimeout(context.Background(), pluginCommandTimeout)
	defer cancel()

	val, err := b.pluginRouter.Route(ctx, op, args)
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

func mustParseInt64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
