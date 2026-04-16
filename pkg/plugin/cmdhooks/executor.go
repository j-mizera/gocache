package cmdhooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	gcpc "gocache/api/gcpc/v1"
	"gocache/api/logger"
	cmd "gocache/pkg/command"
	"gocache/pkg/plugin/router"
	"gocache/pkg/rex"
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
//   - Non-critical hooks fire async (fire-and-forget).
//   - Critical hooks fire sequentially in priority order.
//     If any critical hook returns deny=true, the command is aborted.
//   - On critical hook timeout/error: fail-open (log, continue).
//   - Context values from critical pre-hook responses are accumulated
//     and namespaced by plugin name.
func (e *Executor) RunPreHooks(ctx context.Context, command string, args []string, hookCtx map[string]string) *cmd.PreHookResult {
	matches := e.registry.MatchPre(command)
	if len(matches) == 0 {
		return nil
	}

	metadata := extractRexMetadata(hookCtx)

	// Fire non-critical hooks async (fire-and-forget).
	for _, h := range matches {
		if !h.Critical {
			reqID := router.NextRequestID()
			env := gcpc.NewHookRequest(reqID, gcpc.HookPhaseV1_HOOK_PHASE_PRE, command, args, "", "", cmd.FilterHookCtx(hookCtx, h.PluginName), metadata)
			go h.Conn.SendFireAndForget(env)
		}
	}

	// Fire critical hooks sequentially in priority order.
	for _, h := range matches {
		if !h.Critical {
			continue
		}
		result, err := e.sendCriticalHook(ctx, h, gcpc.HookPhaseV1_HOOK_PHASE_PRE, command, args, "", "", cmd.FilterHookCtx(hookCtx, h.PluginName), metadata)
		if err != nil {
			logger.WarnNoCtx().Str("plugin", h.PluginName).Str("command", command).Err(err).Msg("critical pre-hook failed, allowing command")
			continue
		}
		if result.Deny {
			return &cmd.PreHookResult{Denied: true, DenyReason: result.DenyReason, Context: hookCtx}
		}
		// Merge context values from the response, namespaced.
		cmd.MergeHookCtx(hookCtx, h.PluginName, result.ContextValues)
	}

	return &cmd.PreHookResult{Denied: false, Context: hookCtx}
}

// RunPostHooks fires all matching post-hooks for the command.
//   - Non-critical hooks fire async (fire-and-forget).
//   - Critical hooks fire sequentially (wait for ack, but cannot deny).
func (e *Executor) RunPostHooks(ctx context.Context, command string, args []string, resultValue, resultError string, hookCtx map[string]string) {
	matches := e.registry.MatchPost(command)
	if len(matches) == 0 {
		return
	}

	metadata := extractRexMetadata(hookCtx)

	// Fire non-critical hooks async.
	for _, h := range matches {
		if !h.Critical {
			reqID := router.NextRequestID()
			env := gcpc.NewHookRequest(reqID, gcpc.HookPhaseV1_HOOK_PHASE_POST, command, args, resultValue, resultError, cmd.FilterHookCtx(hookCtx, h.PluginName), metadata)
			go h.Conn.SendFireAndForget(env)
		}
	}

	// Fire critical hooks sequentially (wait for ack).
	for _, h := range matches {
		if !h.Critical {
			continue
		}
		_, err := e.sendCriticalHook(ctx, h, gcpc.HookPhaseV1_HOOK_PHASE_POST, command, args, resultValue, resultError, cmd.FilterHookCtx(hookCtx, h.PluginName), metadata)
		if err != nil {
			logger.WarnNoCtx().Str("plugin", h.PluginName).Str("command", command).Err(err).Msg("critical post-hook failed")
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

// sendCriticalHook sends a hook request and waits for the response (blocking).
func (e *Executor) sendCriticalHook(ctx context.Context, h *HookEntry, phase gcpc.HookPhaseV1, command string, args []string, resultValue, resultError string, filteredCtx map[string]string, metadata map[string]string) (*gcpc.HookResponseV1, error) {
	reqID := router.NextRequestID()
	env := gcpc.NewHookRequest(reqID, phase, command, args, resultValue, resultError, filteredCtx, metadata)

	hookCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	respCh, err := h.Conn.Send(hookCtx, env, reqID)
	if err != nil {
		return nil, fmt.Errorf("send hook: %w", err)
	}

	select {
	case resp, ok := <-respCh:
		if !ok {
			return nil, fmt.Errorf("plugin connection closed")
		}
		hookResp := resp.GetHookResponse()
		if hookResp == nil {
			return nil, fmt.Errorf("unexpected response type")
		}
		return hookResp, nil
	case <-hookCtx.Done():
		h.Conn.DeletePending(reqID)
		return nil, fmt.Errorf("hook timeout")
	}
}
