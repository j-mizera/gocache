package handler

import (
	apicommand "gocache/api/command"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

func HandleMulti(cmdCtx *command.Context) apicommand.Result {
	res, err := cmdCtx.Transaction.Multi(cmdCtx.Client)
	if err != nil {
		return apicommand.Result{Err: err}
	}
	return apicommand.Result{Value: res}
}

func HandleDiscard(cmdCtx *command.Context) apicommand.Result {
	res, err := cmdCtx.Transaction.Discard(cmdCtx.Client)
	if err != nil {
		return apicommand.Result{Err: err}
	}
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}
	return apicommand.Result{Value: res}
}

func HandleExec(cmdCtx *command.Context) apicommand.Result {
	if !cmdCtx.Client.InTransaction {
		return apicommand.Result{Err: clientctx.ErrExecWithoutMulti}
	}

	if cmdCtx.Client.IsWatchDirty() {
		cmdCtx.Client.ResetTransaction()
		if cmdCtx.WatchManager != nil {
			cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
		}
		return apicommand.Result{Value: nil}
	}

	cmdCtx.Client.InTransaction = false
	queue := cmdCtx.Client.CommandQueue
	cmdCtx.Client.CommandQueue = nil

	if queue == nil {
		if cmdCtx.WatchManager != nil {
			cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
		}
		return apicommand.Result{Value: []any{}}
	}

	batchCtx := cmdCtx.Context()
	results, err := cmdCtx.Engine.DispatchWithResult(batchCtx, func() any {
		// Re-check dirty under the lock: a concurrent FLUSHDB may have
		// fired NotifyAll between the pre-lock check and lock acquisition.
		if cmdCtx.Client.IsWatchDirty() {
			return nil
		}
		batchResults := make([]any, len(queue))
		for i, cmdParts := range queue {
			res := cmdCtx.EvalFn(batchCtx, cmdCtx.Client, cmdParts[0], cmdParts[1:], true)
			if res.Err != nil {
				batchResults[i] = res.Err.Error()
			} else {
				batchResults[i] = res.Value
			}
		}
		return batchResults
	})

	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}

	if err != nil {
		return apicommand.Result{Err: err}
	}
	if results == nil {
		return apicommand.Result{Value: nil}
	}
	return apicommand.Result{Value: results}
}

func HandleWatch(cmdCtx *command.Context) apicommand.Result {
	if cmdCtx.Client.InTransaction {
		return apicommand.Result{Value: resp.MarshalError("ERR WATCH inside MULTI is not allowed")}
	}
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Watch(cmdCtx.Client, cmdCtx.Args)
	}
	return apicommand.Result{Value: "OK"}
}

func HandleUnwatch(cmdCtx *command.Context) apicommand.Result {
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}
	return apicommand.Result{Value: "OK"}
}
