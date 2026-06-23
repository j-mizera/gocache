package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apicommand "gocache/api/command"
	"gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	apiobs "gocache/api/observability"
	ops "gocache/api/operations"
	commonobs "gocache/commons/observability"
	"gocache/commons/resp"
	"gocache/pkg/benchstats"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
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
	cache                    *cache.Cache
	engine                   *engine.Engine
	transactionManager       *transaction.Manager
	handlers                 map[string]command.Handler
	specs                    map[string]apicommand.Spec
	requirePass              string
	blockingRegistry         *blocking.Registry
	watchManager             *watch.Manager
	pluginRouter             *router.Router
	hookExecutor             apicommand.HookExecutor
	emitter                  events.Emitter
	operationTrackerManager  *commonobs.SlotOperationTrackerManager
	commandOperationSequence atomic.Uint64
	opHookExecutor           OpHookExecutor
	commandMetrics           CommandMetricsRecorder
	persistenceFeed          command.MutationEmitter
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

// SetOperationTrackerManager wires the commons OperationTracker storage used to
// thread explicit command telemetry scopes into core handlers. Pass nil to keep
// command telemetry disabled while preserving cancellation-only contexts.
func (b *Pipeline) SetOperationTrackerManager(t *commonobs.SlotOperationTrackerManager) {
	b.operationTrackerManager = t
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
			val, err := fn(cmdCtx.CancellationContext(), cmdCtx.Args)
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
	return b.evaluateCore(parentCtx, client, op, args, false, false, commonobs.OperationScope{})
}

// EvaluatePreLocked evaluates a command with ShardLocked=true, indicating
// the target shard's lock is already held by the caller. Used by pipeline
// batch coalescing where shard locks are pre-acquired.
func (b *Pipeline) EvaluatePreLocked(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string, batchTelemetryScope commonobs.OperationScope) apicommand.Result {
	return b.evaluateCore(parentCtx, client, op, args, true, true, batchTelemetryScope)
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
	return b.evaluateCore(parentCtx, ctx, op, args, inBatch, false, commonobs.OperationScope{})
}

func (b *Pipeline) evaluateCore(parentCtx context.Context, ctx *clientctx.ClientContext, op string, args []string, inBatch bool, shardLocked bool, batchTelemetryScope commonobs.OperationScope) apicommand.Result {
	op = strings.ToUpper(op)

	handler, ok := b.handlers[op]
	if !ok {
		// Fall through to plugin router for plugin-provided commands.
		if b.pluginRouter != nil && b.pluginRouter.HasCommand(op) {
			return b.routeToPlugin(parentCtx, ctx, op, args)
		}
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

	// Command telemetry is captured unconditionally. Optional event and hook
	// payloads below still use narrow interest checks before materializing maps or
	// fanout objects, but current subscribers must not decide whether the command
	// operation exists.

	metadata := rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)
	batchTelemetry := inBatch && !batchTelemetryScope.IsZero()
	telemetryScope := batchTelemetryScope
	if !inBatch {
		telemetryScope = b.startCommandTelemetryScope(ctx)
	}
	startNs := time.Now().UnixNano()
	hasHooks := b.hasCommandHookSink(op)

	var cmdOp *ops.Operation
	if b.needsCommandCompatibilityOperation(hasHooks) {
		cmdOp = newCommandCompatibilityOperation(ctx.OperationID, op, len(args), hasSpec && spec.ReadOnly, metadata)
		startNs = cmdOp.StartTime.UnixNano()
	}
	operationID := commandOperationID(telemetryScope, cmdOp)
	if batchTelemetry {
		// Pipeline batching is currently core-command-only because server.canBatch
		// requires Pipeline.SpecFor to resolve the command. If future canBatch logic
		// admits plugin commands, revisit routeToPlugin's startCommandTelemetryScope
		// call so plugin commands do not reopen per-command telemetry operations.
		if b.emitter != nil && b.emitter.HasSubscribersFor(events.CommandStarted) {
			telemetryScope.CommandStartString(op, commandEventFields(operationID, op, args, metadata, 0, "", "")...)
		}
	} else {
		recordCommandStartTelemetryContext(telemetryScope, startNs, operationID, op, len(args), hasSpec && spec.ReadOnly)
	}

	// Build operation-carrying context only for compatibility fanout. The command
	// cancellation context itself remains serverCtx-derived and is not a telemetry
	// carrier.
	opCtx := parentCtx
	if cmdOp != nil {
		opCtx = ops.WithContext(parentCtx, cmdOp)
	}

	// Fire operation start hooks (synchronous — enriches context before work).
	if b.hasCommandOperationHookSink() {
		b.opHookExecutor.RunStartHooks(opCtx, cmdOp)
		if cmdOp != nil && !batchTelemetry {
			recordTelemetryContextMap(telemetryScope, cmdOp.ContextSnapshot(false))
		}
	}

	if !batchTelemetry {
		b.recordCommandStartSignals(telemetryScope, cmdOp, op, args, metadata)
	}

	cmdCtx := cmdCtxPool.Get().(*command.Context)
	defer putCmdCtx(cmdCtx)
	b.fillCmdCtx(cmdCtx, ctx, op, args, inBatch, spec)
	cmdCtx.ShardLocked = shardLocked
	cmdCtx.SetCancellationContext(parentCtx)
	handlerTelemetryScope := telemetryScope
	if batchTelemetry {
		// The batch operation owns the telemetry budget. Handlers can emit
		// diagnostic/cache telemetry through cmdCtx.Telemetry(), but a batched
		// command must contribute only CmdStart + CmdFinish so overflow budgeting
		// can always preserve the batch drop marker and OpFinish records.
		handlerTelemetryScope = commonobs.OperationScope{}
	}
	cmdCtx.SetTelemetry(handlerTelemetryScope)

	// --- Command hooks (pre) ---
	var hookCtx map[string]string
	connInfo := &gcpc.ConnectionInfoV1{Id: ctx.ConnectionID, RemoteAddr: ctx.RemoteAddr}
	cmdInfo := &gcpc.CommandInfoV1{Name: op, Args: args}
	if hasHooks {
		hookCtx = commandProjectionContext(telemetryScope, cmdOp, metadata)

		if pre := b.hookExecutor.RunPreHooks(opCtx, cmdInfo, connInfo, hookCtx); pre != nil {
			if pre.Denied {
				denyReason := "denied: " + pre.DenyReason
				if !batchTelemetry {
					recordTelemetryContextMap(telemetryScope, pre.Context)
					telemetryScope.ContextUpdateStrings(apicommand.ErrorKey, denyReason)
				}
				if cmdOp != nil {
					cmdOp.Fail(denyReason)
				}
				elapsedNs := uint64(time.Now().UnixNano() - startNs)
				if batchTelemetry {
					if b.emitter != nil && b.emitter.HasSubscribersFor(events.CommandCompleted) {
						telemetryScope.CommandFinishString(op, elapsedNs, commandEventFields(operationID, op, args, metadata, elapsedNs, "", denyReason)...)
					}
				} else {
					b.recordCommandFinishSignals(telemetryScope, cmdOp, op, args, metadata, elapsedNs, "", denyReason)
					telemetryScope.Finish(commonobs.SlotTerminalFailed)
				}
				if b.opHookExecutor != nil && cmdOp != nil {
					b.opHookExecutor.RunCompleteHooks(cmdOp)
				}
				return apicommand.Result{Value: resp.MarshalError("DENIED " + pre.DenyReason)}
			}
			hookCtx = pre.Context
			for k, v := range hookCtx {
				cmdOp.Enrich(k, v)
			}
			if !batchTelemetry {
				recordTelemetryContextMap(telemetryScope, hookCtx)
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
	resultVal, resultErr := resultToHookStrings(result)
	elapsedNs := uint64(time.Now().UnixNano() - startNs)
	if cmdOp != nil {
		cmdOp.Complete()
		elapsedNs = uint64(cmdOp.Duration().Nanoseconds())
		cmdOp.Enrich(apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10))
		cmdOp.Enrich(apicommand.ResultKey, resultVal)
		if resultErr != "" {
			cmdOp.Enrich(apicommand.ErrorKey, resultErr)
		}
	}
	b.recordCommandMetric(op, elapsedNs, resultErr != "")
	if !batchTelemetry {
		recordCommandFinishTelemetryContext(telemetryScope, elapsedNs, resultVal, resultErr)
	}

	if b.hasCommandOperationHookSink() && cmdOp != nil {
		b.opHookExecutor.RunCompleteHooks(cmdOp)
	}

	if batchTelemetry {
		if b.emitter != nil && b.emitter.HasSubscribersFor(events.CommandCompleted) {
			telemetryScope.CommandFinishString(op, elapsedNs, commandEventFields(operationID, op, args, metadata, elapsedNs, resultVal, resultErr)...)
		}
	} else {
		b.recordCommandFinishSignals(telemetryScope, cmdOp, op, args, metadata, elapsedNs, resultVal, resultErr)
		finishCommandTelemetryScope(telemetryScope, result)
	}
	return result
}

func (b *Pipeline) recordCommandStartSignals(scope commonobs.OperationScope, cmdOp *ops.Operation, op string, args []string, metadata map[string]string) {
	if b.emitter == nil {
		return
	}
	operationID := commandOperationID(scope, cmdOp)
	if !scope.IsZero() {
		scope.OperationStartString(string(ops.TypeCommand),
			apicommand.OperationID, operationID,
			"_operation_type", string(ops.TypeCommand),
			"_parent_operation_id", commandParentID(scope, cmdOp),
		)
		benchstats.RecordPipelineOperationStarted()
		if b.emitter.HasSubscribersFor(events.CommandStarted) {
			scope.CommandStartString(op, commandEventFields(operationID, op, args, metadata, 0, "", "")...)
		}
		return
	}
	// Runtime event fanout is sidecar-owned. If no sidecar scope exists, keep
	// command execution and hook compatibility working but do not materialize
	// command/operation events directly on the command goroutine.
}

func (b *Pipeline) recordCommandFinishSignals(scope commonobs.OperationScope, cmdOp *ops.Operation, op string, args []string, metadata map[string]string, elapsedNs uint64, resultVal, resultErr string) {
	if b.emitter == nil {
		return
	}
	operationID := commandOperationID(scope, cmdOp)
	if !scope.IsZero() {
		if b.emitter.HasSubscribersFor(events.CommandCompleted) {
			scope.CommandFinishString(op, elapsedNs, commandEventFields(operationID, op, args, metadata, elapsedNs, resultVal, resultErr)...)
		}
		scope.OperationFinishString(string(ops.TypeCommand), elapsedNs,
			apicommand.OperationID, operationID,
			"_operation_type", string(ops.TypeCommand),
			"_status", "completed",
			apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10),
			apicommand.ErrorKey, resultErr,
		)
		benchstats.RecordPipelineOperationCompleted()
		return
	}
	// Runtime event fanout is sidecar-owned. If no sidecar scope exists, keep
	// command execution and hook compatibility working but do not materialize
	// command/operation events directly on the command goroutine.
}

func commandOperationID(scope commonobs.OperationScope, cmdOp *ops.Operation) string {
	if cmdOp != nil && cmdOp.ID != "" {
		return cmdOp.ID
	}
	if !scope.Ref().IsZero() {
		return scope.Ref().ID.String()
	}
	return ""
}

func commandParentID(scope commonobs.OperationScope, cmdOp *ops.Operation) string {
	if cmdOp != nil {
		return cmdOp.ParentID
	}
	return scope.Ref().ParentID.String()
}

func commandEventFields(operationID, op string, args []string, metadata map[string]string, elapsedNs uint64, resultVal, resultErr string) []string {
	fields := make([]string, 0, 8+len(args)*2+len(metadata)*2)
	fields = append(fields,
		apicommand.OperationID, operationID,
		apicommand.CommandKey, op,
		"_args_count", strconv.Itoa(len(args)),
	)
	if elapsedNs > 0 {
		fields = append(fields, apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10))
	}
	if resultVal != "" {
		fields = append(fields, apicommand.ResultKey, resultVal)
	}
	if resultErr != "" {
		fields = append(fields, apicommand.ErrorKey, resultErr)
	}
	for i, arg := range args {
		fields = append(fields, "_arg."+strconv.Itoa(i), arg)
	}
	for key, value := range metadata {
		fields = append(fields, "_metadata."+key, value)
	}
	return fields
}

func (b *Pipeline) needsCommandCompatibilityOperation(hasHooks bool) bool {
	return b.hasCommandOperationHookSink() || hasHooks || (b.hasCommandEventSink() && b.operationTrackerManager == nil)
}

func newCommandCompatibilityOperation(parentID, op string, argCount int, readOnly bool, metadata map[string]string) *ops.Operation {
	cmdOp := ops.New(ops.TypeCommand, parentID)
	startNs := cmdOp.StartTime.UnixNano()
	cmdOp.Enrich(apicommand.StartNs, strconv.FormatInt(startNs, 10))
	cmdOp.Enrich(apicommand.OperationID, cmdOp.ID)
	cmdOp.Enrich(apicommand.CommandKey, op)
	cmdOp.Enrich(apicommand.ArgCountKey, strconv.Itoa(argCount))
	if readOnly {
		cmdOp.Enrich(apicommand.ReadOnlyKey, "true")
	}
	for k, v := range metadata {
		cmdOp.Enrich(rex.Prefix+k, v)
	}
	return cmdOp
}

func (b *Pipeline) startCommandTelemetryScope(ctx *clientctx.ClientContext) commonobs.OperationScope {
	if b.operationTrackerManager == nil {
		return commonobs.OperationScope{}
	}
	sequence := b.commandOperationSequence.Add(1)
	if sequence == 0 {
		sequence = b.commandOperationSequence.Add(1)
	}
	operation := apiobs.InternalOperationIdentity(sequence)
	operationID := "cmd:" + strconv.FormatUint(sequence, 10)
	parent := apiobs.NewParentRef(ctx.OperationID)
	ref := apiobs.NewOperationRef(operationID, ctx.OperationID)
	handle, _, ok := b.operationTrackerManager.StartOperationWithConnectionContextAndMetadata(operation, parent, ctx.ConnectionIdentity, ctx.CmdMeta, commonobs.OperationSnapshotMetadata{
		Type:          string(ops.TypeCommand),
		Ref:           ref,
		StartUnixNano: time.Now().UnixNano(),
	}, &ctx.SlotMagazine)
	if !ok {
		return commonobs.OperationScope{}
	}
	return commonobs.NewOperationScope(b.operationTrackerManager, handle, operation, ref)
}

func finishCommandTelemetryScope(scope commonobs.OperationScope, result apicommand.Result) {
	if resultHasError(result) {
		scope.Finish(commonobs.SlotTerminalFailed)
		return
	}
	scope.Finish(commonobs.SlotTerminalFinished)
}

func commandProjectionContext(scope commonobs.OperationScope, fallback *ops.Operation, metadata map[string]string) map[string]string {
	var projected map[string]string
	if !scope.IsZero() {
		projected = scope.ContextSnapshot()
	}
	if projected == nil && fallback != nil {
		projected = fallback.ContextSnapshot(false)
	}
	if len(metadata) == 0 {
		return projected
	}
	if projected == nil {
		projected = make(map[string]string, len(metadata))
	}
	for key, value := range metadata {
		delete(projected, key)
		projected[rex.Prefix+key] = value
	}
	return projected
}

func recordCommandStartTelemetryContext(scope commonobs.OperationScope, startNs int64, operationID, op string, argCount int, readOnly bool) {
	if scope.IsZero() {
		return
	}
	if readOnly {
		scope.ContextUpdateStrings(
			apicommand.StartNs, strconv.FormatInt(startNs, 10),
			apicommand.OperationID, operationID,
			apicommand.CommandKey, op,
			apicommand.ArgCountKey, strconv.Itoa(argCount),
			apicommand.ReadOnlyKey, "true",
		)
		return
	}
	scope.ContextUpdateStrings(
		apicommand.StartNs, strconv.FormatInt(startNs, 10),
		apicommand.OperationID, operationID,
		apicommand.CommandKey, op,
		apicommand.ArgCountKey, strconv.Itoa(argCount),
	)
}

func recordCommandFinishTelemetryContext(scope commonobs.OperationScope, elapsedNs uint64, resultValue, resultError string) {
	if scope.IsZero() {
		return
	}
	if resultError != "" {
		scope.ContextUpdateStrings(
			apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10),
			apicommand.ResultKey, resultValue,
			apicommand.ErrorKey, resultError,
		)
		return
	}
	scope.ContextUpdateStrings(
		apicommand.ElapsedNs, strconv.FormatUint(elapsedNs, 10),
		apicommand.ResultKey, resultValue,
	)
}

func recordTelemetryContextMap(scope commonobs.OperationScope, values map[string]string) {
	if scope.IsZero() || len(values) == 0 {
		return
	}
	for key, value := range values {
		scope.ContextUpdateStrings(key, value)
	}
}

func (b *Pipeline) recordCommandMetric(op string, elapsedNs uint64, isError bool) {
	if b.hasCommandMetricsSink() {
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
	c.SetOperationTrackerManager(b.operationTrackerManager)
}

// routeToPlugin dispatches a command to a plugin via the router. The per-call
// timeout is derived from parentCtx so connection-level cancellation also
// aborts the plugin call.
func (b *Pipeline) routeToPlugin(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string) apicommand.Result {
	metadata := rex.BuildMetadata(client.RexMeta, client.CmdMeta)
	connInfo := &gcpc.ConnectionInfoV1{Id: client.ConnectionID, RemoteAddr: client.RemoteAddr}

	ctx, cancel := context.WithTimeout(parentCtx, pluginCommandTimeout)
	defer cancel()

	readOnly := false
	if meta := b.pluginRouter.LookupMeta(op); meta != nil && meta.ReadOnly {
		readOnly = true
	}

	cmdOp := newCommandCompatibilityOperation(client.OperationID, op, len(args), readOnly, metadata)
	ctx = ops.WithContext(ctx, cmdOp)
	startNs := cmdOp.StartTime.UnixNano()

	telemetryScope := b.startCommandTelemetryScope(client)
	recordCommandStartTelemetryContext(telemetryScope, startNs, cmdOp.ID, op, len(args), readOnly)
	requestCtx := commandProjectionContext(telemetryScope, cmdOp, metadata)

	val, suppress, err := b.pluginRouter.RouteWithContext(ctx, op, args, metadata, connInfo, requestCtx)
	if err != nil {
		if errors.Is(err, router.ErrPluginTimeout) {
			return b.finishPluginCommand(cmdOp, telemetryScope, apicommand.Result{Value: resp.MarshalError("ERR plugin timeout")})
		}
		if errors.Is(err, router.ErrPluginDown) {
			return b.finishPluginCommand(cmdOp, telemetryScope, apicommand.Result{Value: resp.MarshalError("ERR plugin unavailable")})
		}
		return b.finishPluginCommand(cmdOp, telemetryScope, apicommand.Result{Err: err})
	}
	if e, ok := val.(error); ok {
		return b.finishPluginCommand(cmdOp, telemetryScope, apicommand.Result{Err: e})
	}
	return b.finishPluginCommand(cmdOp, telemetryScope, apicommand.Result{Value: val, SuppressResponse: suppress})
}

func (b *Pipeline) finishPluginCommand(cmdOp *ops.Operation, telemetryScope commonobs.OperationScope, result apicommand.Result) apicommand.Result {
	var elapsedNs uint64
	if cmdOp != nil {
		elapsedNs = uint64(time.Since(cmdOp.StartTime).Nanoseconds())
	}
	resultVal, resultErr := resultToHookStrings(result)
	recordCommandFinishTelemetryContext(telemetryScope, elapsedNs, resultVal, resultErr)
	finishCommandTelemetryScope(telemetryScope, result)
	if cmdOp != nil {
		if resultErr != "" {
			cmdOp.Fail(resultErr)
			return result
		}
		cmdOp.Complete()
	}
	return result
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
