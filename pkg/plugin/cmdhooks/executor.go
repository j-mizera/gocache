package cmdhooks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apicommand "gocache/api/command"
	opctx "gocache/api/context"
	gcpc "gocache/api/gcpc/v1"
	"gocache/commons/logger"
	"gocache/pkg/plugin/router"
	"gocache/pkg/rex"
)

// Sentinel errors returned by the critical hook send path. Callers use
// errors.Is to distinguish them from unexpected failures.
var (
	ErrPluginConnClosed     = errors.New("plugin connection closed")
	ErrUnexpectedResponse   = errors.New("unexpected response type")
	ErrHookTimeout          = errors.New("hook timeout")
)

// Executor dispatches hooks to plugins over IPC.
// It satisfies the command.HookExecutor interface.
type Executor struct {
	registry *Registry
	timeout  time.Duration // deadline for critical (blocking) hooks
}

// NewExecutor creates a hook executor.
func NewExecutor(registry *Registry, timeout time.Duration) *Executor {
	return &Executor{
		registry: registry,
		timeout:  timeout,
	}
}

// HasAny returns true if any hooks are registered. Zero-cost guard.
func (e *Executor) HasAny() bool {
	return e.registry.HasAny()
}

// RunPreHooks fires all matching pre-hooks for the command.
//   - Non-blocking hooks fire async (fire-and-forget).
//   - Blocking hooks fire sequentially in priority order.
//     If any blocking hook returns deny=true, the command is aborted.
//   - On blocking hook timeout/error: critical hooks fail the command,
//     non-critical hooks fail-open (log and continue).
//   - Context values from blocking pre-hook responses are accumulated
//     and namespaced by plugin name.
func (e *Executor) RunPreHooks(ctx context.Context, cmd *gcpc.CommandInfoV1, conn *gcpc.ConnectionInfoV1, hookCtx map[string]string) *apicommand.PreHookResult {
	matches := e.registry.MatchPre(cmd.Name)
	if len(matches) == 0 {
		return nil
	}

	metadata := extractRexMetadata(hookCtx)

	// Fire non-blocking hooks async (fire-and-forget).
	for _, h := range matches {
		if !h.Blocking {
			reqID := router.NextRequestID()
			env := gcpc.NewHookRequest(reqID, gcpc.HookPhaseV1_HOOK_PHASE_PRE, cmd, conn, "", "", opctx.FilterForPlugin(hookCtx, h.PluginName), metadata)
			go h.Conn.SendFireAndForget(env)
		}
	}

	// Fire blocking hooks sequentially in priority order.
	for _, h := range matches {
		if !h.Blocking {
			continue
		}
		result, err := e.sendBlockingHook(ctx, h, gcpc.HookPhaseV1_HOOK_PHASE_PRE, cmd, conn, "", "", opctx.FilterForPlugin(hookCtx, h.PluginName), metadata)
		if err != nil {
			if h.Critical {
				return &apicommand.PreHookResult{Denied: true, DenyReason: fmt.Sprintf("critical hook %s failed: %v", h.PluginName, err), Context: hookCtx}
			}
			logger.Warn(ctx).Str("plugin", h.PluginName).Str("command", cmd.Name).Err(err).Msg("blocking pre-hook failed, allowing command")
			continue
		}
		if result.Deny {
			return &apicommand.PreHookResult{Denied: true, DenyReason: result.DenyReason, Context: hookCtx}
		}
		opctx.MergeFromPlugin(hookCtx, h.PluginName, result.ContextValues)
	}

	return &apicommand.PreHookResult{Denied: false, Context: hookCtx}
}

// RunPostHooks fires all matching post-hooks for the command.
//   - Non-blocking hooks fire async (fire-and-forget).
//   - Blocking hooks fire sequentially (wait for ack, but cannot deny).
func (e *Executor) RunPostHooks(ctx context.Context, cmd *gcpc.CommandInfoV1, conn *gcpc.ConnectionInfoV1, resultValue, resultError string, hookCtx map[string]string) {
	matches := e.registry.MatchPost(cmd.Name)
	if len(matches) == 0 {
		return
	}

	metadata := extractRexMetadata(hookCtx)

	// Fire non-blocking hooks async.
	for _, h := range matches {
		if !h.Blocking {
			reqID := router.NextRequestID()
			env := gcpc.NewHookRequest(reqID, gcpc.HookPhaseV1_HOOK_PHASE_POST, cmd, conn, resultValue, resultError, opctx.FilterForPlugin(hookCtx, h.PluginName), metadata)
			go h.Conn.SendFireAndForget(env)
		}
	}

	// Fire blocking hooks sequentially (wait for ack).
	for _, h := range matches {
		if !h.Blocking {
			continue
		}
		_, err := e.sendBlockingHook(ctx, h, gcpc.HookPhaseV1_HOOK_PHASE_POST, cmd, conn, resultValue, resultError, opctx.FilterForPlugin(hookCtx, h.PluginName), metadata)
		if err != nil {
			logger.Warn(ctx).Str("plugin", h.PluginName).Str("command", cmd.Name).Err(err).Msg("blocking post-hook failed")
		}
	}
}

// extractRexMetadata extracts shared.rex.* keys from a hook context map
// and returns them with bare keys (prefix stripped). Returns nil if none found.
func extractRexMetadata(hookCtx map[string]string) map[string]string {
	var m map[string]string
	for k, v := range hookCtx {
		if strings.HasPrefix(k, rex.Prefix) {
			if m == nil {
				m = make(map[string]string)
			}
			m[k[len(rex.Prefix):]] = v
		}
	}
	return m
}

// sendBlockingHook sends a hook request and waits for the response.
func (e *Executor) sendBlockingHook(ctx context.Context, h *HookEntry, phase gcpc.HookPhaseV1, cmd *gcpc.CommandInfoV1, conn *gcpc.ConnectionInfoV1, resultValue, resultError string, filteredCtx map[string]string, metadata map[string]string) (*gcpc.HookResponseV1, error) {
	reqID := router.NextRequestID()
	env := gcpc.NewHookRequest(reqID, phase, cmd, conn, resultValue, resultError, filteredCtx, metadata)

	hookCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	respCh, err := h.Conn.Send(hookCtx, env, reqID)
	if err != nil {
		return nil, fmt.Errorf("send hook: %w", err)
	}

	select {
	case resp, ok := <-respCh:
		if !ok {
			return nil, ErrPluginConnClosed
		}
		hookResp := resp.GetHookResponse()
		if hookResp == nil {
			return nil, ErrUnexpectedResponse
		}
		return hookResp, nil
	case <-hookCtx.Done():
		h.Conn.DeletePending(reqID)
		return nil, ErrHookTimeout
	}
}
