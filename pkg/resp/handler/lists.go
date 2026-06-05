package handler

import (
	"strconv"
	"time"

	"github.com/gammazero/deque"

	apicommand "gocache/api/command"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/cache/packed"
	"gocache/pkg/command"
)

func listDequeSize(dq *deque.Deque[string]) int64 {
	var size int64
	for i := 0; i < dq.Len(); i++ {
		size += int64(len(dq.At(i))) + cache.ListElementOverhead
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
//   EncNative: entry.Value.(*deque.Deque[string]) — ring-buffer deque used by
//             large lists and lists that have outgrown the packed threshold.
//
// Lists promote on total encoded size (mirroring Valkey's
// list-max-listpack-size default, 8 KiB). Blocking ops (BLPOP/BRPOP) and
// BLPOP wake-ups share the same dual-path.

func HandleLpush(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	values := cmdCtx.Args[1:]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return lpushStartPacked(cmdCtx, key, values)
		}
		if entry.ValueType != cache.ObjTypeList {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			return lpushPacked(cmdCtx, key, cmdCtx.Cache.ResolvePacked(entry), values)
		default:
			return lpushNative(cmdCtx, key, entry.Value.(*deque.Deque[string]), values)
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
		dq, derr := packed.ListToDeque(buf)
		if derr != nil {
			return derr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, dq, listDequeSize(dq), 0); err != nil {
			return err
		}
		return dq.Len()
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Telemetry(), key, cache.ObjTypeList, buf, 0); err != nil {
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
		dq, derr := packed.ListToDeque(newBuf)
		if derr != nil {
			return derr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, dq, listDequeSize(dq), ttl); err != nil {
			return err
		}
		return dq.Len()
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Telemetry(), key, cache.ObjTypeList, newBuf, ttl); err != nil {
		return err
	}
	n, _ := packed.ListLen(newBuf)
	return n
}

func lpushNative(cmdCtx *command.Context, key string, dq *deque.Deque[string], values []string) any {
	for i := len(values) - 1; i >= 0; i-- {
		dq.PushFront(values[i])
	}
	ttl := cmdCtx.Cache.RawTTL(key)
	newSize := cmdCtx.Cache.NativeSize(key) + listAppendSize(values)
	if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, dq, newSize, ttl); err != nil {
		return err
	}
	return dq.Len()
}

func HandleRpush(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	values := cmdCtx.Args[1:]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return rpushStartPacked(cmdCtx, key, values)
		}
		if entry.ValueType != cache.ObjTypeList {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			return rpushPacked(cmdCtx, key, cmdCtx.Cache.ResolvePacked(entry), values)
		default:
			return rpushNative(cmdCtx, key, entry.Value.(*deque.Deque[string]), values)
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
		dq, derr := packed.ListToDeque(buf)
		if derr != nil {
			return derr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, dq, listDequeSize(dq), 0); err != nil {
			return err
		}
		return dq.Len()
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Telemetry(), key, cache.ObjTypeList, buf, 0); err != nil {
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
		dq, derr := packed.ListToDeque(newBuf)
		if derr != nil {
			return derr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, dq, listDequeSize(dq), ttl); err != nil {
			return err
		}
		return dq.Len()
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Telemetry(), key, cache.ObjTypeList, newBuf, ttl); err != nil {
		return err
	}
	n, _ := packed.ListLen(newBuf)
	return n
}

func rpushNative(cmdCtx *command.Context, key string, dq *deque.Deque[string], values []string) any {
	for _, v := range values {
		dq.PushBack(v)
	}
	ttl := cmdCtx.Cache.RawTTL(key)
	newSize := cmdCtx.Cache.NativeSize(key) + listAppendSize(values)
	if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, dq, newSize, ttl); err != nil {
		return err
	}
	return dq.Len()
}

func HandleLpop(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeList {
			return apicommand.ErrWrongType
		}
		val, err := popList(cmdCtx, key, entry, true, cmdCtx.Cache.RawTTL(key))
		if err != nil {
			return err
		}
		return val
	}
	return command.Dispatch(cmdCtx, executeFn)
}

func HandleRpop(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeList {
			return apicommand.ErrWrongType
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
			if werr := cmdCtx.Cache.RawSetPacked(cmdCtx.Telemetry(), key, cache.ObjTypeList, buf, ttl); werr != nil {
				submitHandlerErrorLog(cmdCtx.Telemetry(), "unexpected error on pop write-back", key, werr)
			}
		}
		return string(popped), nil

	default:
		dq := entry.Value.(*deque.Deque[string])
		if dq.Len() == 0 {
			return nil, nil
		}
		var val string
		if fromLeft {
			val = dq.PopFront()
		} else {
			val = dq.PopBack()
		}
		if dq.Len() == 0 {
			cmdCtx.Cache.RawDelete(key)
		} else {
			newSize := cmdCtx.Cache.NativeSize(key) - int64(len(val)) - cache.ListElementOverhead
			if werr := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, dq, newSize, ttl); werr != nil {
				submitHandlerErrorLog(cmdCtx.Telemetry(), "unexpected error on pop write-back", key, werr)
			}
		}
		return val, nil
	}
}

func HandleLlen(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeList {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			n, err := packed.ListLen(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			return n
		default:
			return entry.Value.(*deque.Deque[string]).Len()
		}
	}
	return command.Dispatch(cmdCtx, executeFn)
}

func HandleLRange(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	start, err := strconv.Atoi(cmdCtx.Args[1])
	if err != nil {
		return apicommand.Result{Err: apicommand.ErrNotInteger}
	}
	stop, err := strconv.Atoi(cmdCtx.Args[2])
	if err != nil {
		return apicommand.Result{Err: apicommand.ErrNotInteger}
	}
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeList {
			return apicommand.ErrWrongType
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
			dq := entry.Value.(*deque.Deque[string])
			length := dq.Len()
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
			result := make([]string, 0, stop-start+1)
			for i := start; i <= stop; i++ {
				result = append(result, dq.At(i))
			}
			return result
		}
	}
	return command.Dispatch(cmdCtx, executeFn)
}

func HandleBlpop(cmdCtx *command.Context) apicommand.Result {
	return handleBlockingPop(cmdCtx, true)
}

func HandleBrpop(cmdCtx *command.Context) apicommand.Result {
	return handleBlockingPop(cmdCtx, false)
}

// handleBlockingPop implements the shared logic for BLPOP and BRPOP.
// fromLeft=true pops from the head (BLPOP), fromLeft=false from the tail (BRPOP).
func handleBlockingPop(cmdCtx *command.Context, fromLeft bool) apicommand.Result {
	// Last arg is the timeout in seconds (float64); 0 means block indefinitely.
	timeoutStr := cmdCtx.Args[len(cmdCtx.Args)-1]
	timeoutSec, err := strconv.ParseFloat(timeoutStr, 64)
	if err != nil || timeoutSec < 0 {
		return apicommand.Result{Err: apicommand.ErrInvalidTimeout}
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
		return apicommand.Result{Value: nil}
	}

	if cmdCtx.BlockingRegistry == nil {
		return apicommand.Result{Value: nil}
	}

	// Register interest and wait.
	ch, cancel := cmdCtx.BlockingRegistry.Register(keys)
	defer cancel()

	if timeoutSec == 0 {
		select {
		case wake := <-ch:
			return apicommand.Result{Value: []any{wake.Key, wake.Value}}
		case <-cmdCtx.BlockingRegistry.Done():
			return apicommand.Result{Value: nil}
		}
	}

	timer := time.NewTimer(time.Duration(timeoutSec * float64(time.Second)))
	defer timer.Stop()
	select {
	case wake := <-ch:
		return apicommand.Result{Value: []any{wake.Key, wake.Value}}
	case <-timer.C:
		return apicommand.Result{Value: nil}
	case <-cmdCtx.BlockingRegistry.Done():
		return apicommand.Result{Value: nil}
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
		popResult, dispatchErr := cmdCtx.Engine.DispatchToShard(cmdCtx.Context(), cmdCtx.Cache.ShardIndexOf(key), func() any {
			entry, ok := cmdCtx.Cache.RawGet(key)
			if !ok {
				return nil
			}
			if entry.ValueType != cache.ObjTypeList {
				return nil
			}
			val, err := popList(cmdCtx, key, entry, true, cmdCtx.Cache.RawTTL(key))
			if err != nil {
				submitHandlerErrorLog(cmdCtx.Telemetry(), "blocked-pop pop failed", key, err)
				return nil
			}
			return val
		})
		if dispatchErr != nil {
			submitHandlerErrorLog(cmdCtx.Telemetry(), "blocked-pop dispatch failed", key, dispatchErr)
			return
		}
		if popResult == nil {
			return
		}
		waiterCh <- blocking.WakeResult{Key: key, Value: popResult.(string)}
	}
}
