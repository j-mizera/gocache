package handler

import (
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

func HandleMulti(cmdCtx *command.Context) command.Result {
	res, err := cmdCtx.Transaction.Multi(cmdCtx.Client)
	if err != nil {
		return command.Result{Err: err}
	}
	return command.Result{Value: res}
}

func HandleDiscard(cmdCtx *command.Context) command.Result {
	res, err := cmdCtx.Transaction.Discard(cmdCtx.Client)
	if err != nil {
		return command.Result{Err: err}
	}
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}
	return command.Result{Value: res}
}

func HandleExec(cmdCtx *command.Context) command.Result {
	if !cmdCtx.Client.InTransaction {
		return command.Result{Err: clientctx.ErrExecWithoutMulti}
	}

	if cmdCtx.Client.WatchDirty {
		cmdCtx.Client.ResetTransaction()
		if cmdCtx.WatchManager != nil {
			cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
		}
		return command.Result{Value: nil}
	}

	cmdCtx.Client.InTransaction = false
	queue := cmdCtx.Client.CommandQueue
	cmdCtx.Client.CommandQueue = nil

	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}

	if queue == nil {
		return command.Result{Value: []interface{}{}}
	}

	batchCtx := cmdCtx.Context()
	results, err := cmdCtx.Engine.DispatchWithResult(batchCtx, func() interface{} {
		batchResults := make([]interface{}, len(queue))
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
	if err != nil {
		return command.Result{Err: err}
	}
	return command.Result{Value: results}
}

func HandleWatch(cmdCtx *command.Context) command.Result {
	if cmdCtx.Client.InTransaction {
		return command.Result{Value: resp.MarshalError("ERR WATCH inside MULTI is not allowed")}
	}
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Watch(cmdCtx.Client, cmdCtx.Args)
	}
	return command.Result{Value: "OK"}
}

func HandleUnwatch(cmdCtx *command.Context) command.Result {
	if cmdCtx.WatchManager != nil {
		cmdCtx.WatchManager.Unwatch(cmdCtx.Client)
	}
	return command.Result{Value: "OK"}
}
