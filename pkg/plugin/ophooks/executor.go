package ophooks

import (
	"context"
	"time"

	opctx "gocache/api/context"
	gcpc "gocache/api/gcpc/v1"
	"gocache/api/logger"
	ops "gocache/api/operations"
	"gocache/pkg/plugin/router"
)

// Executor dispatches operation hooks to plugins over IPC.
type Executor struct {
	registry *Registry
	timeout  time.Duration // deadline for start hooks (synchronous)
}

// NewExecutor creates an operation hook executor.
func NewExecutor(registry *Registry, timeout time.Duration) *Executor {
	return &Executor{
		registry: registry,
		timeout:  timeout,
	}
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
		env := gcpc.NewOperationHookRequest(reqID, op.ID, string(op.Type), op.ParentID, "start", filteredCtx)

		hookCtx, cancel := context.WithTimeout(ctx, e.timeout)
		respCh, err := h.Conn.Send(hookCtx, env, reqID)
		if err != nil {
			cancel()
			logger.WarnNoCtx().Str("plugin", h.PluginName).Str("op", op.ID).Err(err).
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
			logger.WarnNoCtx().Str("plugin", h.PluginName).Str("op", op.ID).
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
		env := gcpc.NewOperationHookRequest(reqID, op.ID, string(op.Type), op.ParentID, "complete", filteredCtx)
		go h.Conn.SendFireAndForget(env)
	}
}

// RegistryForTesting returns the underlying registry. Test-only.
func (e *Executor) RegistryForTesting() *Registry {
	return e.registry
}
