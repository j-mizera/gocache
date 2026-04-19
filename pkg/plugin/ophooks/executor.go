package ophooks

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	opctx "gocache/api/context"
	gcpc "gocache/api/gcpc/v1"
	"gocache/api/logger"
	ops "gocache/api/operations"
	apiplugin "gocache/api/plugin"
	"gocache/pkg/operations"
	"gocache/pkg/plugin/router"
)

// Executor dispatches operation hooks to plugins over IPC.
type Executor struct {
	registry *Registry
	timeout  time.Duration // deadline for start hooks (synchronous)

	// Replay dependencies — optional at construction, set by main.go.
	// Replay is a no-op when either is absent, keeping tests and headless
	// integration flows simple.
	mu        sync.RWMutex
	tracker   *operations.Tracker
	startTime time.Time
}

// NewExecutor creates an operation hook executor.
func NewExecutor(registry *Registry, timeout time.Duration) *Executor {
	return &Executor{
		registry: registry,
		timeout:  timeout,
	}
}

// SetTracker wires the operation tracker used for Active-op snapshots
// during replay.
func (e *Executor) SetTracker(t *operations.Tracker) {
	e.mu.Lock()
	e.tracker = t
	e.mu.Unlock()
}

// SetProcessStartTime captures t0 so replay_offset_ns reflects "ns since
// process start when the op actually began" regardless of when the late
// subscriber connected.
func (e *Executor) SetProcessStartTime(t time.Time) {
	e.mu.Lock()
	e.startTime = t
	e.mu.Unlock()
}

// HasAny returns true if any operation hooks are registered. Zero-cost guard.
func (e *Executor) HasAny() bool {
	return e.registry.HasAny()
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
		go h.Conn.SendFireAndForget(env)
	}
}

// Replay synthesizes PhaseStart hooks for every active operation that
// started before regTime and matches the plugin's declared patterns. Sent
// fire-and-forget with Replayed=true — the live operation has already
// passed its enrichment phase, so the plugin cannot affect context this
// late. ReplayOffsetNs is the op's StartTime relative to process start so
// the plugin can position reconstructed spans at the correct wall-clock
// offset instead of at subscribe time.
//
// No-op if tracker is not wired, no active ops match, or the plugin has
// no ophook connection (for example: registration failed midway).
func (e *Executor) Replay(pluginName string, regTime time.Time) {
	e.mu.RLock()
	tracker := e.tracker
	startTime := e.startTime
	e.mu.RUnlock()

	if tracker == nil {
		return
	}
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

	active := tracker.Active()
	// Filter first, sort second — keeps allocation bounded.
	retained := active[:0]
	for _, op := range active {
		if !op.StartTime.Before(regTime) {
			// Op started after the plugin became visible in the
			// registry; live dispatch will deliver the start hook.
			// Skipping here avoids double delivery.
			continue
		}
		if !matchAll && !patternSet[strings.ToLower(string(op.Type))] {
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
		return retained[i].StartTime.Before(retained[j].StartTime)
	})

	for _, op := range retained {
		filteredCtx := op.FilteredContext(pluginName, false)
		reqID := router.NextRequestID()
		offsetNs := int64(0)
		if !startTime.IsZero() {
			offsetNs = op.StartTime.Sub(startTime).Nanoseconds()
			if offsetNs < 0 {
				// Startup log lines emitted before SetProcessStartTime
				// was called shouldn't manifest as negative offsets; a
				// zero offset is a safer "unknown" marker.
				offsetNs = 0
			}
		}
		env := gcpc.NewOperationHookReplay(reqID, op.ID, string(op.Type), op.ParentID, filteredCtx, offsetNs)
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
