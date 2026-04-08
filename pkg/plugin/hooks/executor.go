package hooks

import (
	"context"
	"fmt"
	"time"

	"gocache/pkg/logger"
	"gocache/pkg/plugin/protocol"
	"gocache/pkg/plugin/router"
	gcpc "gocache/proto/gcpc/v1"
)

// PreHookResult reports whether a pre-hook chain denied the command.
type PreHookResult struct {
	Denied     bool
	DenyReason string
}

// Executor dispatches hooks to plugins over IPC.
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
func (e *Executor) RunPreHooks(ctx context.Context, command string, args []string) *PreHookResult {
	matches := e.registry.MatchPre(command)
	if len(matches) == 0 {
		return nil
	}

	// Fire non-critical hooks async (fire-and-forget).
	for _, h := range matches {
		if !h.Critical {
			reqID := router.NextRequestID()
			env := protocol.NewHookRequest(reqID, gcpc.HookPhaseV1_HOOK_PHASE_PRE, command, args, "", "")
			go h.Conn.SendFireAndForget(env)
		}
	}

	// Fire critical hooks sequentially in priority order.
	for _, h := range matches {
		if !h.Critical {
			continue
		}
		result, err := e.sendCriticalHook(ctx, h, gcpc.HookPhaseV1_HOOK_PHASE_PRE, command, args, "", "")
		if err != nil {
			// Fail-open: log and continue.
			logger.Warn().Str("plugin", h.PluginName).Str("command", command).Err(err).Msg("critical pre-hook failed, allowing command")
			continue
		}
		if result.Deny {
			return &PreHookResult{Denied: true, DenyReason: result.DenyReason}
		}
	}

	return &PreHookResult{Denied: false}
}

// RunPostHooks fires all matching post-hooks for the command.
//   - Non-critical hooks fire async (fire-and-forget).
//   - Critical hooks fire sequentially (wait for ack, but cannot deny).
func (e *Executor) RunPostHooks(ctx context.Context, command string, args []string, resultValue, resultError string) {
	matches := e.registry.MatchPost(command)
	if len(matches) == 0 {
		return
	}

	// Fire non-critical hooks async.
	for _, h := range matches {
		if !h.Critical {
			reqID := router.NextRequestID()
			env := protocol.NewHookRequest(reqID, gcpc.HookPhaseV1_HOOK_PHASE_POST, command, args, resultValue, resultError)
			go h.Conn.SendFireAndForget(env)
		}
	}

	// Fire critical hooks sequentially (wait for ack).
	for _, h := range matches {
		if !h.Critical {
			continue
		}
		_, err := e.sendCriticalHook(ctx, h, gcpc.HookPhaseV1_HOOK_PHASE_POST, command, args, resultValue, resultError)
		if err != nil {
			logger.Warn().Str("plugin", h.PluginName).Str("command", command).Err(err).Msg("critical post-hook failed")
		}
	}
}

// sendCriticalHook sends a hook request and waits for the response (blocking).
func (e *Executor) sendCriticalHook(ctx context.Context, h *HookEntry, phase gcpc.HookPhaseV1, command string, args []string, resultValue, resultError string) (*gcpc.HookResponseV1, error) {
	reqID := router.NextRequestID()
	env := protocol.NewHookRequest(reqID, phase, command, args, resultValue, resultError)

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
