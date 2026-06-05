package ophooks

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	opctx "gocache/api/context"
	gcpc "gocache/api/gcpc/v1"
	ops "gocache/api/operations"
	apiplugin "gocache/api/plugin"
	"gocache/commons/logger"
	commonobs "gocache/commons/observability"
	"gocache/pkg/plugin/router"
)

// Executor dispatches operation hooks to plugins over IPC.
type Executor struct {
	registry *Registry
	timeout  time.Duration // deadline for start hooks (synchronous)

	// Replay dependency — optional at construction, set by main.go.
	// Replay is a no-op when telemetry storage is absent, keeping tests and
	// headless integration flows simple.
	mu                      sync.RWMutex
	operationTrackerManager *commonobs.SlotOperationTrackerManager

	// lastReplay remembers when each plugin last received a replay. If
	// it re-registers within minRestartInterval, replay is skipped —
	// crash-looping plugins would otherwise drown in synthetic starts
	// on every reconnect. Zero interval disables suppression.
	lastReplay         map[string]time.Time
	minRestartInterval time.Duration
}

// NewExecutor creates an operation hook executor.
func NewExecutor(registry *Registry, timeout time.Duration) *Executor {
	return &Executor{
		registry:   registry,
		timeout:    timeout,
		lastReplay: make(map[string]time.Time),
	}
}

// SetMinRestartInterval sets the minimum time between replays for a single
// plugin. A re-register within this window skips replay (the plugin is
// assumed to still have the previous replay in flight). Zero disables.
func (e *Executor) SetMinRestartInterval(d time.Duration) {
	e.mu.Lock()
	e.minRestartInterval = d
	e.mu.Unlock()
}

// SetOperationTrackerManager wires telemetry storage used for active-operation
// snapshots during replay. Snapshot materialization runs off the command path.
func (e *Executor) SetOperationTrackerManager(manager *commonobs.SlotOperationTrackerManager) {
	e.mu.Lock()
	e.operationTrackerManager = manager
	e.mu.Unlock()
}

// HasAny returns true if any operation hooks are registered. Zero-cost guard.
func (e *Executor) HasAny() bool {
	return e.registry.HasAny()
}

// HasOperationType reports whether any operation hook matches opType.
func (e *Executor) HasOperationType(opType ops.Type) bool {
	return e.registry.HasOperationType(opType)
}

// RunStartHooks fires operation start hooks synchronously in priority order.
// Each hook can enrich the operation context. Timeout per hook, fail-open on timeout.
func (e *Executor) RunStartHooks(ctx context.Context, op *ops.Operation) {
	matches := e.registry.Match(op.Type)
	if len(matches) == 0 {
		return
	}

	for _, h := range matches {
		filteredCtx := op.FilteredContext(h.PluginName, false)
		reqID := router.NextRequestID()
		env := gcpc.NewOperationHookRequest(reqID, op.ID, string(op.Type), op.ParentID, apiplugin.PhaseStart, filteredCtx)

		hookCtx, cancel := context.WithTimeout(ctx, e.timeout)
		respCh, err := h.Conn.Send(hookCtx, env, reqID)
		if err != nil {
			cancel()
			logger.Warn(ctx).Str("plugin", h.PluginName).Str("op", op.ID).Err(err).
				Msg("operation start hook send failed, continuing")
			continue
		}

		select {
		case resp, ok := <-respCh:
			cancel()
			if !ok {
				continue
			}
			hookResp := resp.GetOperationHookResponse()
			if hookResp != nil && len(hookResp.ContextValues) > 0 {
				// Auto-prefix non-shared keys with plugin name, then merge into operation.
				merged := make(map[string]string, len(hookResp.ContextValues))
				opctx.MergeFromPlugin(merged, h.PluginName, hookResp.ContextValues)
				op.EnrichMany(merged)
			}
		case <-hookCtx.Done():
			cancel()
			h.Conn.DeletePending(reqID)
			logger.Warn(ctx).Str("plugin", h.PluginName).Str("op", op.ID).
				Msg("operation start hook timed out, continuing")
		}
	}
}

// RunCompleteHooks fires operation complete hooks asynchronously (fire-and-forget).
func (e *Executor) RunCompleteHooks(op *ops.Operation) {
	matches := e.registry.Match(op.Type)
	if len(matches) == 0 {
		return
	}

	for _, h := range matches {
		filteredCtx := op.FilteredContext(h.PluginName, false)
		reqID := router.NextRequestID()
		env := gcpc.NewOperationHookRequest(reqID, op.ID, string(op.Type), op.ParentID, apiplugin.PhaseComplete, filteredCtx)
		h.Conn.SendFireAndForget(env)
	}
}

// Replay synthesizes PhaseStart hooks for every active operation that
// started before regTime and matches the plugin's declared patterns. Sent
// fire-and-forget with Replayed=true — the live operation has already
// passed its enrichment phase, so the plugin cannot affect context this
// late. ReplayStartUnixNs is the op's absolute wall-clock start (Unix ns),
// ready for the plugin to pass straight into OTEL/Jaeger as the span
// start time — no server-side anchor knowledge required.
//
// No-op if tracker is not wired, no active ops match, or the plugin has
// no ophook connection (for example: registration failed midway).
func (e *Executor) Replay(pluginName string, regTime time.Time) {
	e.mu.Lock()
	manager := e.operationTrackerManager
	interval := e.minRestartInterval
	last, hadPrior := e.lastReplay[pluginName]
	if manager == nil {
		e.mu.Unlock()
		return
	}
	if hadPrior && interval > 0 && regTime.Sub(last) < interval {
		e.mu.Unlock()
		logger.InfoNoCtx().
			Str("plugin", pluginName).
			Dur("since_last_replay", regTime.Sub(last)).
			Dur("min_interval", interval).
			Msg("skipping ophook replay — plugin re-registered inside restart-storm window")
		return
	}
	// Record regTime before dropping the lock so a second near-simultaneous
	// Register can't sneak past the suppression check.
	e.lastReplay[pluginName] = regTime
	e.mu.Unlock()

	conn := e.registry.ConnFor(pluginName)
	if conn == nil {
		return
	}

	patterns := e.registry.PatternsFor(pluginName)
	if len(patterns) == 0 {
		return
	}
	patternSet := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		patternSet[p] = true
	}
	matchAll := patternSet["*"]

	active := manager.ActiveOperationSnapshots()
	// Filter first, sort second — keeps allocation bounded to replay processing.
	retained := active[:0]
	for _, op := range active {
		if op.StartUnixNano == 0 || !time.Unix(0, op.StartUnixNano).Before(regTime) {
			// Op started after the plugin became visible in the registry, or
			// lacks a telemetry start timestamp; live dispatch covers the former
			// and replay must not guess the latter.
			continue
		}
		if op.Type == "" {
			continue
		}
		if !matchAll && !patternSet[strings.ToLower(op.Type)] {
			continue
		}
		retained = append(retained, op)
	}
	if len(retained) == 0 {
		return
	}

	// Deliver in start-time order so span reconstruction sees parents
	// before children (parent ops always start before children).
	sort.Slice(retained, func(i, j int) bool {
		return retained[i].StartUnixNano < retained[j].StartUnixNano
	})

	for _, op := range retained {
		filteredCtx := opctx.FilterForPlugin(op.Context, pluginName)
		reqID := router.NextRequestID()
		// Absolute wall-clock start lets the plugin place the reconstructed
		// span at its real occurrence time without any shared reference point
		// with the server.
		env := gcpc.NewOperationHookReplay(reqID, op.Ref.ID.String(), op.Type, op.Ref.ParentID.String(), filteredCtx, op.StartUnixNano)
		// Synchronous send preserves start-time order over the wire — span
		// reconstruction on the plugin side depends on parents arriving
		// before children.
		conn.SendFireAndForget(env)
	}

	logger.InfoNoCtx().
		Str("plugin", pluginName).
		Int("replayed", len(retained)).
		Int("active", len(active)).
		Msg("replayed active operation hooks to new subscriber")
}
