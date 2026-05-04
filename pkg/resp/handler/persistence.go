package handler

import (
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
