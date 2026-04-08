package evaluator

import (
	"gocache/pkg/logger"
	"gocache/pkg/persistence"
)

func (b *BaseEvaluator) handleSnapshot(cmdCtx *CommandContext) Result {
	executeFn := func() interface{} {
		if err := persistence.SaveSnapshot(b.snapshotFile, cmdCtx.Cache); err != nil {
			return err
		}
		return "OK"
	}
	res := dispatch(cmdCtx, executeFn)
	if res.Err != nil {
		logger.Error().Err(res.Err).Msg("snapshot command failed")
	}
	return res
}

func (b *BaseEvaluator) handleLoadSnapshot(cmdCtx *CommandContext) Result {
	filename := cmdCtx.Args[0]
	executeFn := func() interface{} {
		if err := persistence.LoadSnapshot(filename, cmdCtx.Cache); err != nil {
			return err
		}
		return "OK"
	}
	res := dispatch(cmdCtx, executeFn)
	if res.Err != nil {
		logger.Error().Err(res.Err).Str("file", filename).Msg("loadsnapshot command failed")
	}
	return res
}
