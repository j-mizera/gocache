package pipeline

import (
	"context"
	"fmt"
	"testing"

	apicommand "gocache/api/command"
	gcpc "gocache/api/gcpc/v1"
	"gocache/pkg/clientctx"
)

// noOpHook is a single no-op PRE hook function. It does no work and returns
// nil (no denial). The benchmark measures DISPATCH overhead, not hook-body
// execution, so the hook body must be trivially cheap.
type noOpHook func(ctx context.Context, cmd *gcpc.CommandInfoV1, conn *gcpc.ConnectionInfoV1) *apicommand.PreHookResult

// countingHookExecutor implements command.HookExecutor with a configurable
// number of no-op hooks. It simulates the dispatch overhead of N hooks
// without actual plugin IPC — the delta between hook counts isolates the
// per-hook marginal dispatch cost.
type countingHookExecutor struct {
	hooks []noOpHook
}

func (executor *countingHookExecutor) HasAny() bool { return len(executor.hooks) > 0 }

func (executor *countingHookExecutor) HasHooksForCommand(string) bool {
	return len(executor.hooks) > 0
}

func (executor *countingHookExecutor) RunPreHooks(ctx context.Context, cmd *gcpc.CommandInfoV1, conn *gcpc.ConnectionInfoV1, hookCtx map[string]string) *apicommand.PreHookResult {
	// No hooks registered: nil signals "no matching hooks" per HookExecutor.
	if len(executor.hooks) == 0 {
		return nil
	}

	for _, hook := range executor.hooks {
		preHookOutcome := hook(ctx, cmd, conn)
		if preHookOutcome != nil && preHookOutcome.Denied {
			return preHookOutcome
		}
	}

	return &apicommand.PreHookResult{Context: hookCtx}
}

func (executor *countingHookExecutor) RunPostHooks(context.Context, *gcpc.CommandInfoV1, *gcpc.ConnectionInfoV1, string, string, map[string]string) {
}

// BenchmarkHookDispatchRTT measures full pipeline Evaluate RTT with no-op PRE
// hooks registered at multiple counts. Per-hook marginal cost =
// (RTT_N - RTT_0) / N. This isolates dispatch overhead from hook-body
// execution. Absolute RTT excludes socket transport, which is constant and
// cancels in the delta.
func BenchmarkHookDispatchRTT(b *testing.B) {
	hookCounts := []int{0, 1, 4, 16}
	for _, hookCount := range hookCounts {
		benchmarkName := "NoHook"
		if hookCount > 0 {
			benchmarkName = fmt.Sprintf("PreHook_%d", hookCount)
		}
		b.Run(benchmarkName, func(b *testing.B) {
			evaluator, engineInstance, _ := newTestPipeline()
			defer engineInstance.Stop()

			hooks := make([]noOpHook, hookCount)
			for hookIndex := range hooks {
				hooks[hookIndex] = func(_ context.Context, _ *gcpc.CommandInfoV1, _ *gcpc.ConnectionInfoV1) *apicommand.PreHookResult {
					return nil
				}
			}
			evaluator.SetHookExecutor(&countingHookExecutor{hooks: hooks})

			clientContext := clientctx.New()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = evaluator.Evaluate(context.Background(), clientContext, "PING", nil)
			}
		})
	}
}
