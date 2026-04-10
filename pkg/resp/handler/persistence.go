package handler

import (
	"gocache/pkg/command"
	"gocache/pkg/logger"
	"gocache/pkg/persistence"
)

func HandleSnapshot(cmdCtx *command.Context) command.Result {
	executeFn := func() interface{} {
		if err := persistence.SaveSnapshot(cmdCtx.SnapshotFile, cmdCtx.Cache); err != nil {
			return err
		}
		return "OK"
	}
	res := command.Dispatch(cmdCtx, executeFn)
	if res.Err != nil {
		logger.Error().Err(res.Err).Msg("snapshot command failed")
	}
	return res
}

func HandleLoadSnapshot(cmdCtx *command.Context) command.Result {
	filename := cmdCtx.Args[0]
	executeFn := func() interface{} {
		if err := persistence.LoadSnapshot(filename, cmdCtx.Cache); err != nil {
			return err
		}
		return "OK"
	}
	res := command.Dispatch(cmdCtx, executeFn)
	if res.Err != nil {
		logger.Error().Err(res.Err).Str("file", filename).Msg("loadsnapshot command failed")
	}
	return res
}
