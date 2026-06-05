package pipeline

import (
	"gocache/api/events"
	ops "gocache/api/operations"
)

func (b *Pipeline) hasCommandHookSink(op string) bool {
	return b.hookExecutor != nil && b.hookExecutor.HasHooksForCommand(op)
}

func (b *Pipeline) hasCommandOperationHookSink() bool {
	return b.opHookExecutor != nil && b.opHookExecutor.HasOperationType(ops.TypeCommand)
}

func (b *Pipeline) hasCommandEventSink() bool {
	return b.emitter != nil && b.emitter.HasSubscribersFor(
		events.OperationStarted,
		events.OperationCompleted,
		events.CommandStarted,
		events.CommandCompleted,
	)
}

func (b *Pipeline) hasCommandMetricsSink() bool {
	return b.commandMetrics != nil && b.commandMetrics.HasCommandMetricsSink()
}
