package evaluator

import (
	"gocache/pkg/clientctx"
	"gocache/pkg/resp"
)

func (b *BaseEvaluator) handleMulti(cmdCtx *CommandContext) Result {
	res, err := cmdCtx.Transaction.Multi(cmdCtx.Client)
	if err != nil {
		return Result{Err: err}
	}
	return Result{Value: res}
}

func (b *BaseEvaluator) handleDiscard(cmdCtx *CommandContext) Result {
	res, err := cmdCtx.Transaction.Discard(cmdCtx.Client)
	if err != nil {
		return Result{Err: err}
	}
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}
	return Result{Value: res}
}

func (b *BaseEvaluator) handleExec(cmdCtx *CommandContext) Result {
	if !cmdCtx.Client.InTransaction {
		return Result{Err: clientctx.ErrExecWithoutMulti}
	}

	// Check if any watched key was modified — abort if dirty.
	if cmdCtx.Client.WatchDirty {
		cmdCtx.Client.ResetTransaction()
		if cmdCtx.WatchManager != nil {
			cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
		}
		return Result{Value: nil}
	}

	cmdCtx.Client.InTransaction = false
	queue := cmdCtx.Client.CommandQueue
	cmdCtx.Client.CommandQueue = nil

	// Always clear watches after EXEC.
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}

	if queue == nil {
		return Result{Value: []interface{}{}}
	}

	// Execute all commands atomically in the engine
	results := cmdCtx.Engine.DispatchWithResult(func() interface{} {
		batchResults := make([]interface{}, len(queue))
		for i, cmdParts := range queue {
			res := b.evaluateInternal(cmdCtx.Client, cmdParts[0], cmdParts[1:], true)
			if res.Err != nil {
				batchResults[i] = res.Err.Error()
			} else {
				batchResults[i] = res.Value
			}
		}
		return batchResults
	})
	return Result{Value: results}
}

// WATCH key [key ...]
func (b *BaseEvaluator) handleWatch(cmdCtx *CommandContext) Result {
	if cmdCtx.Client.InTransaction {
		return Result{Value: resp.MarshalError("ERR WATCH inside MULTI is not allowed")}
	}
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Watch(cmdCtx.Client, cmdCtx.Args)
	}
	return Result{Value: "OK"}
}

// UNWATCH
func (b *BaseEvaluator) handleUnwatch(cmdCtx *CommandContext) Result {
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}
	return Result{Value: "OK"}
}
