package evaluator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/logger"
	"gocache/pkg/plugin/hooks"
	"gocache/pkg/plugin/router"
	"gocache/pkg/resp"
	"gocache/pkg/transaction"
	"gocache/pkg/watch"
)

// pluginCommandTimeout is the maximum time to wait for a plugin to respond.
const pluginCommandTimeout = 10 * time.Second

// ErrInvalidDuration is returned when a duration argument cannot be parsed.
var ErrInvalidDuration = errors.New("invalid duration")

type Result struct {
	Value interface{}
	Err   error
}

type CommandContext struct {
	Client           *clientctx.ClientContext
	Op               string
	Args             []string
	InBatch          bool
	Engine           *engine.Engine
	Cache            *cache.Cache
	Transaction      *transaction.Manager
	BlockingRegistry *blocking.Registry
	WatchManager     *watch.Manager
}

type CommandHandler func(cmdCtx *CommandContext) Result

// commandSpec defines the minimum and maximum number of arguments a command
// accepts (not counting the command name itself). max == -1 means unlimited.
type commandSpec struct {
	min int
	max int
}

var commandSpecs = map[string]commandSpec{
	resp.CmdSet:          {2, -1},
	resp.CmdGet:          {1, 1},
	resp.CmdDelete:       {1, -1},
	resp.CmdExists:       {1, 1},
	resp.CmdExpire:       {2, 2},
	resp.CmdTTL:          {1, 1},
	resp.CmdLPush:        {2, -1},
	resp.CmdRPush:        {2, -1},
	resp.CmdLPop:         {1, 1},
	resp.CmdRPop:         {1, 1},
	resp.CmdLLen:         {1, 1},
	resp.CmdLRange:       {3, 3},
	resp.CmdBLPop:        {2, -1},
	resp.CmdBRPop:        {2, -1},
	resp.CmdHSet:         {3, -1},
	resp.CmdHGet:         {2, 2},
	resp.CmdHDel:         {2, -1},
	resp.CmdHExists:      {2, 2},
	resp.CmdHGetAll:      {1, 1},
	resp.CmdHKeys:        {1, 1},
	resp.CmdHVals:        {1, 1},
	resp.CmdHLen:         {1, 1},
	resp.CmdSAdd:         {2, -1},
	resp.CmdSRem:         {2, -1},
	resp.CmdSMembers:     {1, 1},
	resp.CmdSIsMember:    {2, 2},
	resp.CmdSCard:        {1, 1},
	resp.CmdSPop:         {1, 1},
	resp.CmdZAdd:         {3, -1},
	resp.CmdZRem:         {2, -1},
	resp.CmdZScore:       {2, 2},
	resp.CmdZCard:        {1, 1},
	resp.CmdZRange:       {3, 4},
	resp.CmdZRank:        {2, 2},
	resp.CmdZCount:       {3, 3},
	resp.CmdMulti:        {0, 0},
	resp.CmdExec:         {0, 0},
	resp.CmdDiscard:      {0, 0},
	resp.CmdSnapshot:     {0, 0},
	resp.CmdLoadSnapshot: {1, 1},
	resp.CmdDBSize:       {0, 0},
	resp.CmdInfo:         {0, 1},
	resp.CmdHello:        {1, 1},

	// Server/connection commands
	resp.CmdPing:     {0, 1},
	resp.CmdEcho:     {1, 1},
	resp.CmdSelect:   {1, 1},
	resp.CmdFlushDB:  {0, 0},
	resp.CmdFlushAll: {0, 0},
	resp.CmdAuth:     {1, 1},

	// String counter commands
	resp.CmdIncr:        {1, 1},
	resp.CmdDecr:        {1, 1},
	resp.CmdIncrBy:      {2, 2},
	resp.CmdDecrBy:      {2, 2},
	resp.CmdIncrByFloat: {2, 2},
	resp.CmdAppend:      {2, 2},
	resp.CmdStrlen:      {1, 1},

	// Multi-key commands
	resp.CmdMGet: {1, -1},
	resp.CmdMSet: {2, -1},

	// SET variants and TTL commands
	resp.CmdSetNX:   {2, 2},
	resp.CmdPExpire: {2, 2},
	resp.CmdPTTL:    {1, 1},

	// Set operations
	resp.CmdSInter: {1, -1},
	resp.CmdSUnion: {1, -1},
	resp.CmdSDiff:  {1, -1},

	// Key management commands
	resp.CmdType:      {1, 1},
	resp.CmdRename:    {2, 2},
	resp.CmdRenameNX:  {2, 2},
	resp.CmdKeys:      {1, 1},
	resp.CmdScan:      {1, -1},
	resp.CmdRandomKey: {0, 0},

	// Watch commands
	resp.CmdWatch:   {1, -1},
	resp.CmdUnwatch: {0, 0},

	// Key introspection
	resp.CmdObject: {1, 2},
}

type Evaluator interface {
	Evaluate(ctx *clientctx.ClientContext, op string, args []string) Result
	RegisterHandler(op string, handler CommandHandler)
	SetPluginRouter(r *router.Router)
	SetHookExecutor(e *hooks.Executor)
	CoreCommandNames() []string
}

type BaseEvaluator struct {
	cache              *cache.Cache
	engine             *engine.Engine
	transactionManager *transaction.Manager
	handlers           map[string]CommandHandler
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
		handlers:           make(map[string]CommandHandler),
		snapshotFile:       snapshotFile,
		requirePass:        requirePass,
		blockingRegistry:   br,
		watchManager:       wm,
	}
	b.registerHandlers()
	return b
}

func (b *BaseEvaluator) RegisterHandler(op string, handler CommandHandler) {
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
	// Basic commands
	b.handlers[resp.CmdSet] = b.handleSet
	b.handlers[resp.CmdGet] = b.handleGet
	b.handlers[resp.CmdDelete] = b.handleDelete
	b.handlers[resp.CmdExists] = b.handleExists
	b.handlers[resp.CmdExpire] = b.handleExpire
	b.handlers[resp.CmdPExpire] = b.handlePexpire
	b.handlers[resp.CmdTTL] = b.handleTtl
	b.handlers[resp.CmdPTTL] = b.handlePttl
	b.handlers[resp.CmdSetNX] = b.handleSetnx

	// List commands
	b.handlers[resp.CmdLPush] = b.handleLpush
	b.handlers[resp.CmdRPush] = b.handleRpush
	b.handlers[resp.CmdLPop] = b.handleLpop
	b.handlers[resp.CmdRPop] = b.handleRpop
	b.handlers[resp.CmdLLen] = b.handleLlen
	b.handlers[resp.CmdLRange] = b.handleLrange
	b.handlers[resp.CmdBLPop] = b.handleBlpop
	b.handlers[resp.CmdBRPop] = b.handleBrpop

	// Hash commands
	b.handlers[resp.CmdHSet] = b.handleHset
	b.handlers[resp.CmdHGet] = b.handleHget
	b.handlers[resp.CmdHDel] = b.handleHdel
	b.handlers[resp.CmdHExists] = b.handleHexists
	b.handlers[resp.CmdHGetAll] = b.handleHgetall
	b.handlers[resp.CmdHKeys] = b.handleHkeys
	b.handlers[resp.CmdHVals] = b.handleHvals
	b.handlers[resp.CmdHLen] = b.handleHlen

	// Set commands
	b.handlers[resp.CmdSAdd] = b.handleSadd
	b.handlers[resp.CmdSRem] = b.handleSrem
	b.handlers[resp.CmdSMembers] = b.handleSmembers
	b.handlers[resp.CmdSIsMember] = b.handleSismember
	b.handlers[resp.CmdSCard] = b.handleScard
	b.handlers[resp.CmdSPop] = b.handleSpop
	b.handlers[resp.CmdSInter] = b.handleSinter
	b.handlers[resp.CmdSUnion] = b.handleSunion
	b.handlers[resp.CmdSDiff] = b.handleSdiff

	// Sorted Set commands
	b.handlers[resp.CmdZAdd] = b.handleZadd
	b.handlers[resp.CmdZRem] = b.handleZrem
	b.handlers[resp.CmdZScore] = b.handleZscore
	b.handlers[resp.CmdZCard] = b.handleZcard
	b.handlers[resp.CmdZRange] = b.handleZrange
	b.handlers[resp.CmdZRank] = b.handleZrank
	b.handlers[resp.CmdZCount] = b.handleZcount

	// Transaction commands
	b.handlers[resp.CmdMulti] = b.handleMulti
	b.handlers[resp.CmdDiscard] = b.handleDiscard
	b.handlers[resp.CmdExec] = b.handleExec

	// Persistence commands
	b.handlers[resp.CmdSnapshot] = b.handleSnapshot
	b.handlers[resp.CmdLoadSnapshot] = b.handleLoadSnapshot

	// Server commands
	b.handlers[resp.CmdDBSize] = b.handleDbsize
	b.handlers[resp.CmdInfo] = b.handleInfo
	b.handlers[resp.CmdHello] = b.handleHello
	b.handlers[resp.CmdPing] = b.handlePing
	b.handlers[resp.CmdEcho] = b.handleEcho
	b.handlers[resp.CmdSelect] = b.handleSelect
	b.handlers[resp.CmdFlushDB] = b.handleFlushDB
	b.handlers[resp.CmdFlushAll] = b.handleFlushAll
	b.handlers[resp.CmdAuth] = b.handleAuth

	// String counter commands
	b.handlers[resp.CmdIncr] = b.handleIncr
	b.handlers[resp.CmdDecr] = b.handleDecr
	b.handlers[resp.CmdIncrBy] = b.handleIncrBy
	b.handlers[resp.CmdDecrBy] = b.handleDecrBy
	b.handlers[resp.CmdIncrByFloat] = b.handleIncrByFloat
	b.handlers[resp.CmdAppend] = b.handleAppend
	b.handlers[resp.CmdStrlen] = b.handleStrlen

	// Multi-key commands
	b.handlers[resp.CmdMGet] = b.handleMget
	b.handlers[resp.CmdMSet] = b.handleMset

	// Key management commands
	b.handlers[resp.CmdType] = b.handleType
	b.handlers[resp.CmdRename] = b.handleRename
	b.handlers[resp.CmdRenameNX] = b.handleRenameNX
	b.handlers[resp.CmdKeys] = b.handleKeys
	b.handlers[resp.CmdScan] = b.handleScan
	b.handlers[resp.CmdRandomKey] = b.handleRandomKey

	// Watch commands
	b.handlers[resp.CmdWatch] = b.handleWatch
	b.handlers[resp.CmdUnwatch] = b.handleUnwatch

	// Key introspection
	b.handlers[resp.CmdObject] = b.handleObject
}

func (b *BaseEvaluator) Evaluate(ctx *clientctx.ClientContext, op string, args []string) Result {
	return b.evaluateInternal(ctx, op, args, false)
}

func (b *BaseEvaluator) evaluateInternal(ctx *clientctx.ClientContext, op string, args []string, inBatch bool) Result {
	op = strings.ToUpper(op)

	handler, ok := b.handlers[op]
	if !ok {
		// Fall through to plugin router for plugin-provided commands.
		if b.pluginRouter != nil && b.pluginRouter.HasCommand(op) {
			return b.routeToPlugin(op, args)
		}
		logger.Debug().Str("command", op).Msg("unknown command")
		return Result{Value: resp.ErrUnknown(strings.ToLower(op))}
	}

	if spec, hasSpec := commandSpecs[op]; hasSpec {
		n := len(args)
		if n < spec.min || (spec.max >= 0 && n > spec.max) {
			return Result{Value: resp.ErrArgs(strings.ToLower(op))}
		}
	}

	// Transactional logic: queue commands if in transaction, except for transaction control commands
	if ctx.InTransaction && !inBatch {
		if op != resp.CmdMulti && op != resp.CmdExec && op != resp.CmdDiscard && op != resp.CmdHello {
			if op == "QUIT" {
				return Result{Value: "OK"}
			}
			ctx.EnqueueCommand(append([]string{op}, args...))
			return Result{Value: "QUEUED"}
		}
	}

	cmdCtx := &CommandContext{
		Client:           ctx,
		Op:               op,
		Args:             args,
		InBatch:          inBatch,
		Engine:           b.engine,
		Cache:            b.cache,
		Transaction:      b.transactionManager,
		BlockingRegistry: b.blockingRegistry,
		WatchManager:     b.watchManager,
	}

	// Pre-hooks: fire before command execution.
	if b.hookExecutor != nil && b.hookExecutor.HasAny() {
		if pre := b.hookExecutor.RunPreHooks(context.Background(), op, args); pre != nil && pre.Denied {
			return Result{Value: resp.MarshalError("DENIED " + pre.DenyReason)}
		}
	}

	result := handler(cmdCtx)

	// Post-hooks: fire after command execution.
	if b.hookExecutor != nil && b.hookExecutor.HasAny() {
		resultVal, resultErr := resultToHookStrings(result)
		b.hookExecutor.RunPostHooks(context.Background(), op, args, resultVal, resultErr)
	}

	return result
}

// routeToPlugin dispatches a command to a plugin via the router.
func (b *BaseEvaluator) routeToPlugin(op string, args []string) Result {
	// Use a background context with a generous timeout; the router
	// handles per-request timeouts internally if needed.
	ctx, cancel := context.WithTimeout(context.Background(), pluginCommandTimeout)
	defer cancel()

	val, err := b.pluginRouter.Route(ctx, op, args)
	if err != nil {
		if errors.Is(err, router.ErrPluginTimeout) {
			return Result{Value: resp.MarshalError("ERR plugin timeout")}
		}
		if errors.Is(err, router.ErrPluginDown) {
			return Result{Value: resp.MarshalError("ERR plugin unavailable")}
		}
		return Result{Value: resp.MarshalError("ERR " + err.Error())}
	}
	// If the plugin returned an error as the value, propagate it.
	if e, ok := val.(error); ok {
		return Result{Err: e}
	}
	return Result{Value: val}
}

// resultToHookStrings extracts a string representation of a Result for post-hooks.
func resultToHookStrings(r Result) (string, string) {
	if r.Err != nil {
		return "", r.Err.Error()
	}
	if r.Value == nil {
		return "", ""
	}
	return fmt.Sprintf("%v", r.Value), ""
}

// dispatch runs fn either directly (inBatch) or through the engine dispatcher,
// then wraps the result into a Result, propagating any error.
func dispatch(cmdCtx *CommandContext, fn func() interface{}) Result {
	var res interface{}
	if cmdCtx.InBatch {
		res = fn()
	} else {
		res = cmdCtx.Engine.DispatchWithResult(fn)
	}
	if err, ok := res.(error); ok {
		return Result{Err: err}
	}
	return Result{Value: res}
}
