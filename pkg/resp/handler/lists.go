package handler

import (
	"errors"
	"strconv"
	"time"

	"gocache/api/logger"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// ErrInvalidTimeout is returned by BLPOP/BRPOP when the timeout argument
// is not a valid non-negative float.
var ErrInvalidTimeout = errors.New("timeout is not a float or out of range")

func HandleLpush(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	values := cmdCtx.Args[1:]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		var list []string
		if !found {
			list = []string{}
		} else {
			if entry.ValueType != cache.ObjTypeList {
				return resp.ErrWrongType
			}
			list = entry.Value.([]string)
		}

		reversed := make([]string, len(values))
		for i, v := range values {
			reversed[len(values)-1-i] = v
		}
		list = append(reversed, list...)
		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, list, 0); err != nil {
			return err
		}
		return len(list)
	}
	result := command.Dispatch(cmdCtx, executeFn)
	if result.Err == nil {
		tryWakeBlockedClients(cmdCtx, key)
	}
	return result
}

func HandleRpush(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	values := cmdCtx.Args[1:]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		var list []string
		if !found {
			list = []string{}
		} else {
			if entry.ValueType != cache.ObjTypeList {
				return resp.ErrWrongType
			}
			list = entry.Value.([]string)
		}

		list = append(list, values...)
		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, list, 0); err != nil {
			return err
		}
		return len(list)
	}
	result := command.Dispatch(cmdCtx, executeFn)
	if result.Err == nil {
		tryWakeBlockedClients(cmdCtx, key)
	}
	return result
}

func HandleLpop(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeList {
			return resp.ErrWrongType
		}
		list := entry.Value.([]string)
		if len(list) == 0 {
			return nil
		}
		val := list[0]
		list = list[1:]
		if len(list) == 0 {
			cmdCtx.Cache.RawDelete(key)
		} else {
			if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, list, 0); err != nil {
				return err
			}
		}
		return val
	}
	return command.Dispatch(cmdCtx, executeFn)
}

func HandleRpop(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeList {
			return resp.ErrWrongType
		}
		list := entry.Value.([]string)
		if len(list) == 0 {
			return nil
		}
		val := list[len(list)-1]
		list = list[:len(list)-1]
		if len(list) == 0 {
			cmdCtx.Cache.RawDelete(key)
		} else {
			if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, list, 0); err != nil {
				return err
			}
		}
		return val
	}
	return command.Dispatch(cmdCtx, executeFn)
}

func HandleLlen(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeList {
			return resp.ErrWrongType
		}
		return len(entry.Value.([]string))
	}
	return command.Dispatch(cmdCtx, executeFn)
}

func HandleLRange(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	start, err := strconv.Atoi(cmdCtx.Args[1])
	if err != nil {
		return command.Result{Err: resp.ErrNotInteger}
	}
	stop, err := strconv.Atoi(cmdCtx.Args[2])
	if err != nil {
		return command.Result{Err: resp.ErrNotInteger}
	}
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeList {
			return resp.ErrWrongType
		}
		list := entry.Value.([]string)
		length := len(list)

		if start < 0 {
			start = length + start
		}
		if stop < 0 {
			stop = length + stop
		}
		if start < 0 {
			start = 0
		}
		if stop >= length {
			stop = length - 1
		}
		if start > stop || start >= length {
			return []string{}
		}
		return list[start : stop+1]
	}
	return command.Dispatch(cmdCtx, executeFn)
}

func HandleBlpop(cmdCtx *command.Context) command.Result {
	return handleBlockingPop(cmdCtx, true)
}

func HandleBrpop(cmdCtx *command.Context) command.Result {
	return handleBlockingPop(cmdCtx, false)
}

// handleBlockingPop implements the shared logic for BLPOP and BRPOP.
// fromLeft=true pops from the head (BLPOP), fromLeft=false from the tail (BRPOP).
func handleBlockingPop(cmdCtx *command.Context, fromLeft bool) command.Result {
	// Last arg is the timeout in seconds (float64); 0 means block indefinitely.
	timeoutStr := cmdCtx.Args[len(cmdCtx.Args)-1]
	timeoutSec, err := strconv.ParseFloat(timeoutStr, 64)
	if err != nil || timeoutSec < 0 {
		return command.Result{Err: ErrInvalidTimeout}
	}
	keys := cmdCtx.Args[:len(cmdCtx.Args)-1]

	// Phase 1: attempt an immediate non-blocking pop.
	result := command.Dispatch(cmdCtx, func() any {
		for _, key := range keys {
			entry, found := cmdCtx.Cache.RawGet(key)
			if !found {
				continue
			}
			// Skip expired keys.
			if lazyExpire(cmdCtx.Cache, key) {
				continue
			}
			if entry.ValueType != cache.ObjTypeList {
				continue
			}
			list := entry.Value.([]string)
			if len(list) == 0 {
				continue
			}
			var val string
			if fromLeft {
				val = list[0]
				list = list[1:]
			} else {
				val = list[len(list)-1]
				list = list[:len(list)-1]
			}
			if len(list) == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				// Shrinking write — RawSet cannot return ErrOutOfMemory because
				// delta ≤ 0, but surface any unexpected error instead of dropping it.
				if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, list, cmdCtx.Cache.RawTTL(key)); err != nil {
					logger.Error(cmdCtx.Context()).Err(err).Str("key", key).Msg("unexpected error on pop write-back")
				}
			}
			return []any{key, val}
		}
		return nil
	})

	if result.Value != nil {
		return result
	}

	// Phase 2: do not block inside a MULTI/EXEC batch.
	if cmdCtx.InBatch {
		return command.Result{Value: nil}
	}

	if cmdCtx.BlockingRegistry == nil {
		return command.Result{Value: nil}
	}

	// Register interest and wait.
	ch, cancel := cmdCtx.BlockingRegistry.Register(keys)
	defer cancel()

	if timeoutSec == 0 {
		// Block indefinitely until woken or server shuts down.
		select {
		case wake := <-ch:
			return command.Result{Value: []any{wake.Key, wake.Value}}
		case <-cmdCtx.BlockingRegistry.Done():
			return command.Result{Value: nil}
		}
	}

	timer := time.NewTimer(time.Duration(timeoutSec * float64(time.Second)))
	defer timer.Stop()
	select {
	case wake := <-ch:
		return command.Result{Value: []any{wake.Key, wake.Value}}
	case <-timer.C:
		return command.Result{Value: nil}
	case <-cmdCtx.BlockingRegistry.Done():
		return command.Result{Value: nil}
	}
}

// tryWakeBlockedClients pops one element for each blocked client that is
// waiting on key, sending it the result through the registry channel.
// It must NOT be called while the engine lock is held by the caller.
func tryWakeBlockedClients(cmdCtx *command.Context, key string) {
	if cmdCtx.BlockingRegistry == nil {
		return
	}
	for {
		waiterCh, found := cmdCtx.BlockingRegistry.TryWake(key)
		if !found {
			return
		}
		popResult, dispatchErr := cmdCtx.Engine.DispatchWithResult(cmdCtx.Context(), func() any {
			entry, ok := cmdCtx.Cache.RawGet(key)
			if !ok {
				return nil
			}
			if entry.ValueType != cache.ObjTypeList {
				return nil
			}
			list := entry.Value.([]string)
			if len(list) == 0 {
				return nil
			}
			val := list[0]
			list = list[1:]
			if len(list) == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				// Shrinking write — RawSet cannot return ErrOutOfMemory because
				// delta ≤ 0, but surface any unexpected error instead of dropping it.
				if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, list, cmdCtx.Cache.RawTTL(key)); err != nil {
					logger.Error(cmdCtx.Context()).Err(err).Str("key", key).Msg("unexpected error on blocked-pop write-back")
				}
			}
			return val
		})
		if dispatchErr != nil {
			logger.Error(cmdCtx.Context()).Err(dispatchErr).Str("key", key).Msg("blocked-pop dispatch failed")
			return
		}
		if popResult == nil {
			return
		}
		waiterCh <- blocking.WakeResult{Key: key, Value: popResult.(string)}
	}
}
