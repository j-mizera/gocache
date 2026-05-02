package evaluator

// hasAnySink reports whether any observer is currently attached to the
// evaluator's three sinks: the event bus, the command-hook executor, and
// the operation-hook executor.
//
// All three checks are atomic loads (see pkg/events.Bus.HasSubscribers,
// pkg/plugin/cmdhooks.Registry.HasAny, pkg/plugin/ophooks.Registry.HasAny)
// so calling this on every command does not contend on a lock. It exists
// solely to gate the fast path in evaluateInternal — when nothing is
// listening, the entire instrumentation block (tracker register/unregister,
// 7× operation enrich, 4× event emit, pre/post hook chains, ContextSnapshot
// allocations) is dead weight and is skipped wholesale.
//
// Documented invariant: a subscriber that attaches mid-flight does NOT see
// lifecycle events for commands that started under the fast path. The
// event bus replay ring covers Emit'd events; ophook lifecycle replay is
// out of scope (see ophooks.Executor.Replay — it only fires for ops the
// tracker still holds, which the fast path does not register).
func (b *BaseEvaluator) hasAnySink() bool {
	if b.emitter != nil && b.emitter.HasSubscribers() {
		return true
	}
	if b.hookExecutor != nil && b.hookExecutor.HasAny() {
		return true
	}
	if b.opHookExecutor != nil && b.opHookExecutor.HasAny() {
		return true
	}
	return false
}
