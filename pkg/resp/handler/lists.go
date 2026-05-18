package handler

import (
	"strconv"
	"time"

	"gocache/api/logger"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/cache/packed"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)


// listSliceSize walks a []string once to compute its estimateSize-equivalent
// payload size. Used at the packed→native promotion boundary; subsequent
// mutations track size incrementally and never re-walk.
func listSliceSize(list []string) int64 {
	var size int64
	for _, s := range list {
		size += int64(len(s)) + cache.ListElementOverhead
	}
	return size
}

// listAppendSize returns the byte cost added by appending values to a list.
func listAppendSize(values []string) int64 {
	var size int64
	for _, v := range values {
		size += int64(len(v)) + cache.ListElementOverhead
	}
	return size
}

// List commands operate on two encodings:
//
//   EncPacked: cmdCtx.Cache.ResolvePacked(entry) — a packed.List buffer, bounded by
//             PackedThresholds.ListMaxBytes. Mutations splice in place.
//   EncNative: entry.Value.([]string) — the Go slice used by large lists
//             and lists that have outgrown the packed threshold.
//
// Lists promote on total encoded size (mirroring Valkey's
// list-max-listpack-size default, 8 KiB). Blocking ops (BLPOP/BRPOP) and
// BLPOP wake-ups share the same dual-path.

func HandleLpush(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	values := cmdCtx.Args[1:]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return lpushStartPacked(cmdCtx, key, values)
		}
		if entry.ValueType != cache.ObjTypeList {
			return resp.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			return lpushPacked(cmdCtx, key, cmdCtx.Cache.ResolvePacked(entry), values)
		default:
			return lpushNative(cmdCtx, key, entry.Value.([]string), values)
		}
	}
	result := command.Dispatch(cmdCtx, executeFn)
	if result.Err == nil {
		tryWakeBlockedClients(cmdCtx, key)
	}
	return result
}

func lpushStartPacked(cmdCtx *command.Context, key string, values []string) any {
	buf, promoted, err := packed.ListAppendLeft(packed.ListNew(), values, cmdCtx.Cache.PackedThresholds().ListMaxBytes)
	if err != nil {
		return err
	}
	if promoted {
		items, derr := packed.ListToSlice(buf)
		if derr != nil {
			return derr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, items, listSliceSize(items), 0); err != nil {
			return err
		}
		return len(items)
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeList, buf, 0); err != nil {
		return err
	}
	n, _ := packed.ListLen(buf)
	return n
}

func lpushPacked(cmdCtx *command.Context, key string, buf []byte, values []string) any {
	ttl := cmdCtx.Cache.RawTTL(key)
	newBuf, promoted, err := packed.ListAppendLeft(buf, values, cmdCtx.Cache.PackedThresholds().ListMaxBytes)
	if err != nil {
		return err
	}
	if promoted {
		items, derr := packed.ListToSlice(newBuf)
		if derr != nil {
			return derr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, items, listSliceSize(items), ttl); err != nil {
			return err
		}
		return len(items)
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeList, newBuf, ttl); err != nil {
		return err
	}
	n, _ := packed.ListLen(newBuf)
	return n
}

func lpushNative(cmdCtx *command.Context, key string, list []string, values []string) any {
	reversed := make([]string, len(values))
	for i, v := range values {
		reversed[len(values)-1-i] = v
	}
	list = append(reversed, list...)
	ttl := cmdCtx.Cache.RawTTL(key)
	newSize := cmdCtx.Cache.NativeSize(key) + listAppendSize(values)
	if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, list, newSize, ttl); err != nil {
		return err
	}
	return len(list)
}

func HandleRpush(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	values := cmdCtx.Args[1:]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return rpushStartPacked(cmdCtx, key, values)
		}
		if entry.ValueType != cache.ObjTypeList {
			return resp.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			return rpushPacked(cmdCtx, key, cmdCtx.Cache.ResolvePacked(entry), values)
		default:
			return rpushNative(cmdCtx, key, entry.Value.([]string), values)
		}
	}
	result := command.Dispatch(cmdCtx, executeFn)
	if result.Err == nil {
		tryWakeBlockedClients(cmdCtx, key)
	}
	return result
}

func rpushStartPacked(cmdCtx *command.Context, key string, values []string) any {
	buf, promoted, err := packed.ListAppendRight(packed.ListNew(), values, cmdCtx.Cache.PackedThresholds().ListMaxBytes)
	if err != nil {
		return err
	}
	if promoted {
		items, derr := packed.ListToSlice(buf)
		if derr != nil {
			return derr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, items, listSliceSize(items), 0); err != nil {
			return err
		}
		return len(items)
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeList, buf, 0); err != nil {
		return err
	}
	n, _ := packed.ListLen(buf)
	return n
}

func rpushPacked(cmdCtx *command.Context, key string, buf []byte, values []string) any {
	ttl := cmdCtx.Cache.RawTTL(key)
	newBuf, promoted, err := packed.ListAppendRight(buf, values, cmdCtx.Cache.PackedThresholds().ListMaxBytes)
	if err != nil {
		return err
	}
	if promoted {
		items, derr := packed.ListToSlice(newBuf)
		if derr != nil {
			return derr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, items, listSliceSize(items), ttl); err != nil {
			return err
		}
		return len(items)
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeList, newBuf, ttl); err != nil {
		return err
	}
	n, _ := packed.ListLen(newBuf)
	return n
}

func rpushNative(cmdCtx *command.Context, key string, list []string, values []string) any {
	list = append(list, values...)
	ttl := cmdCtx.Cache.RawTTL(key)
	newSize := cmdCtx.Cache.NativeSize(key) + listAppendSize(values)
	if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, list, newSize, ttl); err != nil {
		return err
	}
	return len(list)
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
		val, err := popList(cmdCtx, key, entry, true, cmdCtx.Cache.RawTTL(key))
		if err != nil {
			return err
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
		val, err := popList(cmdCtx, key, entry, false, cmdCtx.Cache.RawTTL(key))
		if err != nil {
			return err
		}
		return val
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// popList removes and returns the leftmost (fromLeft=true) or rightmost
// (fromLeft=false) element. Returns nil-any when the list is empty. ttl is
// the existing expiration in nanoseconds that must be preserved on
// write-back; 0 means no TTL.
func popList(cmdCtx *command.Context, key string, entry cache.Entry, fromLeft bool, ttl int64) (any, error) {
	switch entry.Encoding {
	case cache.EncPacked:
		buf := cmdCtx.Cache.ResolvePacked(entry)
		var popped []byte
		var ok bool
		var err error
		if fromLeft {
			buf, popped, ok, err = packed.ListPopLeft(buf)
		} else {
			buf, popped, ok, err = packed.ListPopRight(buf)
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		n, _ := packed.ListLen(buf)
		if n == 0 {
			cmdCtx.Cache.RawDelete(key)
		} else {
			if werr := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeList, buf, ttl); werr != nil {
				logger.Error(cmdCtx.Context()).Err(werr).Str("key", key).Msg("unexpected error on pop write-back")
			}
		}
		return string(popped), nil

	default:
		list := entry.Value.([]string)
		if len(list) == 0 {
			return nil, nil
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
			newSize := cmdCtx.Cache.NativeSize(key) - int64(len(val)) - cache.ListElementOverhead
			if werr := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, list, newSize, ttl); werr != nil {
				logger.Error(cmdCtx.Context()).Err(werr).Str("key", key).Msg("unexpected error on pop write-back")
			}
		}
		return val, nil
	}
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
		switch entry.Encoding {
		case cache.EncPacked:
			n, err := packed.ListLen(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			return n
		default:
			return len(entry.Value.([]string))
		}
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
		switch entry.Encoding {
		case cache.EncPacked:
			items, err := packed.ListRange(cmdCtx.Cache.ResolvePacked(entry), start, stop)
			if err != nil {
				return err
			}
			if items == nil {
				return []string{}
			}
			return items
		default:
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
		return command.Result{Err: resp.ErrInvalidTimeout}
	}
	keys := cmdCtx.Args[:len(cmdCtx.Args)-1]
	cmdCtx.TouchedShards = cmdCtx.Cache.TouchedShards(keys)

	// Phase 1: attempt an immediate non-blocking pop.
	result := command.Dispatch(cmdCtx, func() any {
		for _, key := range keys {
			entry, found := cmdCtx.Cache.RawGet(key)
			if !found {
				continue
			}
			if lazyExpire(cmdCtx.Cache, key) {
				continue
			}
			if entry.ValueType != cache.ObjTypeList {
				continue
			}
			val, err := popList(cmdCtx, key, entry, fromLeft, cmdCtx.Cache.RawTTL(key))
			if err != nil {
				return err
			}
			if val == nil {
				continue
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
			val, err := popList(cmdCtx, key, entry, true, cmdCtx.Cache.RawTTL(key))
			if err != nil {
				logger.Error(cmdCtx.Context()).Err(err).Str("key", key).Msg("blocked-pop pop failed")
				return nil
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
