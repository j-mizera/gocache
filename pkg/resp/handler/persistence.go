package handler

import (
	"context"
	"fmt"

	"gocache/api/logger"
	"gocache/pkg/command"
)

// HandleSnapshot handles SAVE / SNAPSHOT — triggers an immediate
// snapshot save through the registered persistence plugin (via
// cmdCtx.Snapshotter, which is the Coordinator). The plugin owns
// where and how to write; this handler only dispatches.
func HandleSnapshot(cmdCtx *command.Context) command.Result {
	executeFn := func() any {
		if cmdCtx.Snapshotter == nil {
			return fmt.Errorf("snapshot: no snapshotter registered")
		}
		if err := cmdCtx.Snapshotter.Snapshot(cmdCtx.Context(), cmdCtx.Cache); err != nil {
			return err
		}
		return "OK"
	}
	res := command.Dispatch(cmdCtx, executeFn)
	if res.Err != nil {
		logger.Error(cmdCtx.Context()).Err(res.Err).Msg("snapshot command failed")
	}
	return res
}

// HandleSave is the Redis-compatible alias for HandleSnapshot.
func HandleSave(cmdCtx *command.Context) command.Result {
	return HandleSnapshot(cmdCtx)
}

// HandleBgsave triggers an asynchronous snapshot in a background
// goroutine and returns immediately. The response is sent before the
// snapshot completes — clients that need confirmation should poll
// LASTSAVE.
func HandleBgsave(cmdCtx *command.Context) command.Result {
	if cmdCtx.Snapshotter == nil {
		return command.Result{Err: fmt.Errorf("snapshot: no snapshotter registered")}
	}
	snapshotter := cmdCtx.Snapshotter
	c := cmdCtx.Cache
	go func() {
		if err := snapshotter.Snapshot(context.Background(), c); err != nil {
			logger.ErrorNoCtx().Err(err).Msg("BGSAVE failed")
		}
	}()
	return command.Result{Value: "Background saving started"}
}

// HandleLastsave returns the Unix timestamp (seconds) of the last
// successful snapshot save. Returns 0 if no save has completed since
// server start.
func HandleLastsave(cmdCtx *command.Context) command.Result {
	if cmdCtx.Snapshotter == nil {
		return command.Result{Err: fmt.Errorf("snapshot: no snapshotter registered")}
	}
	return command.Result{Value: cmdCtx.Snapshotter.LastSaveUnix()}
}
