package evaluator

import (
	"errors"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/resp"
	"strconv"
	"time"
)

func (b *BaseEvaluator) handleLpush(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	values := cmdCtx.Args[1:]
	executeFn := func() interface{} {
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
		if err := cmdCtx.Cache.RawSet(key, list, 0); err != nil {
			return err
		}
		return len(list)
	}
	result := dispatch(cmdCtx, executeFn)
	if result.Err == nil {
		b.tryWakeBlockedClients(cmdCtx, key)
	}
	return result
}

func (b *BaseEvaluator) handleRpush(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	values := cmdCtx.Args[1:]
	executeFn := func() interface{} {
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
		if err := cmdCtx.Cache.RawSet(key, list, 0); err != nil {
			return err
		}
		return len(list)
	}
	result := dispatch(cmdCtx, executeFn)
	if result.Err == nil {
		b.tryWakeBlockedClients(cmdCtx, key)
	}
	return result
}

func (b *BaseEvaluator) handleLpop(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
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
			if err := cmdCtx.Cache.RawSet(key, list, 0); err != nil {
				return err
			}
		}
		return val
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleRpop(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
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
			if err := cmdCtx.Cache.RawSet(key, list, 0); err != nil {
				return err
			}
		}
		return val
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleLlen(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeList {
			return resp.ErrWrongType
		}
		return len(entry.Value.([]string))
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleLrange(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	start, err := strconv.Atoi(cmdCtx.Args[1])
	if err != nil {
		return Result{Err: resp.ErrNotInteger}
	}
	stop, err := strconv.Atoi(cmdCtx.Args[2])
	if err != nil {
		return Result{Err: resp.ErrNotInteger}
	}
	executeFn := func() interface{} {
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
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleBlpop(cmdCtx *CommandContext) Result {
	return b.handleBlockingPop(cmdCtx, true)
}

func (b *BaseEvaluator) handleBrpop(cmdCtx *CommandContext) Result {
	return b.handleBlockingPop(cmdCtx, false)
}

// handleBlockingPop implements the shared logic for BLPOP and BRPOP.
// fromLeft=true pops from the head (BLPOP), fromLeft=false from the tail (BRPOP).
func (b *BaseEvaluator) handleBlockingPop(cmdCtx *CommandContext, fromLeft bool) Result {
	// Last arg is the timeout in seconds (float64); 0 means block indefinitely.
	timeoutStr := cmdCtx.Args[len(cmdCtx.Args)-1]
	timeoutSec, err := strconv.ParseFloat(timeoutStr, 64)
	if err != nil || timeoutSec < 0 {
		return Result{Err: errors.New("timeout is not a float or out of range")}
	}
	keys := cmdCtx.Args[:len(cmdCtx.Args)-1]

	// Phase 1: attempt an immediate non-blocking pop.
	result := dispatch(cmdCtx, func() interface{} {
		for _, key := range keys {
			entry, found := cmdCtx.Cache.RawGet(key)
			if !found {
				continue
			}
			// Skip expired keys.
			_, state := cmdCtx.Cache.TTLInternal(key)
			if state == cache.ValueExpired {
				cmdCtx.Cache.RawDelete(key)
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
				_ = cmdCtx.Cache.RawSet(key, list, cmdCtx.Cache.RawTTL(key))
			}
			return []interface{}{key, val}
		}
		return nil
	})

	if result.Value != nil {
		return result
	}

	// Phase 2: do not block inside a MULTI/EXEC batch.
	if cmdCtx.InBatch {
		return Result{Value: nil}
	}

	if cmdCtx.BlockingRegistry == nil {
		return Result{Value: nil}
	}

	// Register interest and wait.
	ch, cancel := cmdCtx.BlockingRegistry.Register(keys)
	defer cancel()

	if timeoutSec == 0 {
		// Block indefinitely until woken or server shuts down.
		select {
		case wake := <-ch:
			return Result{Value: []interface{}{wake.Key, wake.Value}}
		case <-cmdCtx.BlockingRegistry.Done():
			return Result{Value: nil}
		}
	}

	timer := time.NewTimer(time.Duration(timeoutSec * float64(time.Second)))
	defer timer.Stop()
	select {
	case wake := <-ch:
		return Result{Value: []interface{}{wake.Key, wake.Value}}
	case <-timer.C:
		return Result{Value: nil}
	case <-cmdCtx.BlockingRegistry.Done():
		return Result{Value: nil}
	}
}

// tryWakeBlockedClients pops one element for each blocked client that is
// waiting on key, sending it the result through the registry channel.
// It must NOT be called while the engine lock is held by the caller.
func (b *BaseEvaluator) tryWakeBlockedClients(cmdCtx *CommandContext, key string) {
	if b.blockingRegistry == nil {
		return
	}
	for {
		waiterCh, found := b.blockingRegistry.TryWake(key)
		if !found {
			return
		}
		popResult := cmdCtx.Engine.DispatchWithResult(func() interface{} {
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
				_ = cmdCtx.Cache.RawSet(key, list, cmdCtx.Cache.RawTTL(key))
			}
			return val
		})
		if popResult == nil {
			return
		}
		waiterCh <- blocking.WakeResult{Key: key, Value: popResult.(string)}
	}
}
