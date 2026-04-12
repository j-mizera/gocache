package command

import "context"

// PreHookResult reports whether a pre-hook chain denied the command
// and carries the accumulated hook context.
type PreHookResult struct {
	Denied     bool
	DenyReason string
	Context    map[string]string // accumulated context from pre-hooks
}

// HookExecutor is the interface the evaluator uses to dispatch hooks.
// Implementations may be backed by a real plugin IPC bus, an in-memory
// chain for tests, or a no-op.
type HookExecutor interface {
	// HasAny reports whether any hooks are registered. Used as a zero-cost
	// guard so the evaluator can skip hook context construction entirely
	// when there are no listeners.
	HasAny() bool

	// RunPreHooks fires all matching pre-hooks for the command. Returns nil
	// if there are no matching hooks. Otherwise returns a PreHookResult that
	// indicates whether the command was denied and carries the accumulated
	// hook context after all pre-hooks have run.
	RunPreHooks(ctx context.Context, op string, args []string, hookCtx map[string]string) *PreHookResult

	// RunPostHooks fires all matching post-hooks for the command. Post-hooks
	// observe the command result but cannot abort it.
	RunPostHooks(ctx context.Context, op string, args []string, resultValue, resultError string, hookCtx map[string]string)
}
