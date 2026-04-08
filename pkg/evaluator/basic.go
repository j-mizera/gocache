package evaluator

import (
	"fmt"
	"gocache/pkg/cache"
	"gocache/pkg/resp"
	"strconv"
	"strings"
	"time"
)

// handlePing returns PONG with no arguments, or echoes the first argument.
func (b *BaseEvaluator) handlePing(cmdCtx *CommandContext) Result {
	if len(cmdCtx.Args) == 0 {
		return Result{Value: "PONG"}
	}
	return Result{Value: cmdCtx.Args[0]}
}

// handleEcho returns the single argument as-is.
func (b *BaseEvaluator) handleEcho(cmdCtx *CommandContext) Result {
	return Result{Value: cmdCtx.Args[0]}
}

// handleSelect accepts only DB 0; any other index is an error.
func (b *BaseEvaluator) handleSelect(cmdCtx *CommandContext) Result {
	idx, err := strconv.Atoi(cmdCtx.Args[0])
	if err != nil || idx != 0 {
		return Result{Value: resp.MarshalError("ERR DB index is out of range")}
	}
	return Result{Value: "OK"}
}

// handleFlushDB clears the entire cache (single-DB server, same as FLUSHALL).
func (b *BaseEvaluator) handleFlushDB(cmdCtx *CommandContext) Result {
	return dispatch(cmdCtx, func() interface{} {
		cmdCtx.Cache.Clear()
		return "OK"
	})
}

// handleFlushAll clears the entire cache.
func (b *BaseEvaluator) handleFlushAll(cmdCtx *CommandContext) Result {
	return dispatch(cmdCtx, func() interface{} {
		cmdCtx.Cache.Clear()
		return "OK"
	})
}

// handleAuth validates a password against requirePass.
func (b *BaseEvaluator) handleAuth(cmdCtx *CommandContext) Result {
	if b.requirePass == "" {
		return Result{Value: resp.MarshalError("ERR Client sent AUTH, but no password is set")}
	}
	if cmdCtx.Args[0] != b.requirePass {
		return Result{Value: resp.MarshalError("WRONGPASS invalid username-password pair")}
	}
	cmdCtx.Client.Authenticated = true
	return Result{Value: "OK"}
}

// handleIncr atomically increments the integer value stored at key by 1.
func (b *BaseEvaluator) handleIncr(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	return dispatch(cmdCtx, func() interface{} {
		return incrByDelta(cmdCtx, key, 1)
	})
}

// handleDecr atomically decrements the integer value stored at key by 1.
func (b *BaseEvaluator) handleDecr(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	return dispatch(cmdCtx, func() interface{} {
		return incrByDelta(cmdCtx, key, -1)
	})
}

// handleIncrBy increments by the supplied integer delta.
func (b *BaseEvaluator) handleIncrBy(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	delta, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil {
		return Result{Value: resp.ErrNotIntegerValue()}
	}
	return dispatch(cmdCtx, func() interface{} {
		return incrByDelta(cmdCtx, key, delta)
	})
}

// handleDecrBy decrements by the supplied integer delta.
func (b *BaseEvaluator) handleDecrBy(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	delta, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil {
		return Result{Value: resp.ErrNotIntegerValue()}
	}
	return dispatch(cmdCtx, func() interface{} {
		return incrByDelta(cmdCtx, key, -delta)
	})
}

// handleIncrByFloat increments the float value stored at key.
func (b *BaseEvaluator) handleIncrByFloat(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	incr, err := strconv.ParseFloat(cmdCtx.Args[1], 64)
	if err != nil {
		return Result{Value: resp.ErrNotFloatValue()}
	}
	return dispatch(cmdCtx, func() interface{} {
		_, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(key)
		}

		existing := 0.0
		if entry, found := cmdCtx.Cache.RawGet(key); found {
			str, ok := entry.Value.(string)
			if !ok {
				return resp.ErrWrongType
			}
			existing, err = strconv.ParseFloat(str, 64)
			if err != nil {
				return resp.ErrNotFloat
			}
		}

		newVal := existing + incr
		newStr := strconv.FormatFloat(newVal, 'f', -1, 64)
		rawTTL := cmdCtx.Cache.RawTTL(key)
		if setErr := cmdCtx.Cache.RawSet(key, newStr, rawTTL); setErr != nil {
			return setErr
		}
		return newStr
	})
}

// handleAppend appends value to the string stored at key, creating it if absent.
// Returns the new length of the string.
func (b *BaseEvaluator) handleAppend(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	suffix := cmdCtx.Args[1]
	return dispatch(cmdCtx, func() interface{} {
		_, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(key)
		}

		existing := ""
		rawTTL := int64(0)
		if entry, found := cmdCtx.Cache.RawGet(key); found {
			str, ok := entry.Value.(string)
			if !ok {
				return resp.ErrWrongType
			}
			existing = str
			rawTTL = cmdCtx.Cache.RawTTL(key)
		}

		newStr := existing + suffix
		if setErr := cmdCtx.Cache.RawSet(key, newStr, rawTTL); setErr != nil {
			return setErr
		}
		return int64(len(newStr))
	})
}

// handleStrlen returns the length of the string stored at key, or 0 if absent.
func (b *BaseEvaluator) handleStrlen(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	return dispatch(cmdCtx, func() interface{} {
		_, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(key)
			return int64(0)
		}

		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return int64(0)
		}
		str, ok := entry.Value.(string)
		if !ok {
			return resp.ErrWrongType
		}
		return int64(len(str))
	})
}

// handleMget returns the values for all specified keys (nil for absent/non-string).
func (b *BaseEvaluator) handleMget(cmdCtx *CommandContext) Result {
	keys := cmdCtx.Args
	return dispatch(cmdCtx, func() interface{} {
		result := make([]interface{}, len(keys))
		for i, key := range keys {
			_, state := cmdCtx.Cache.TTLInternal(key)
			if state == cache.ValueExpired {
				cmdCtx.Cache.RawDelete(key)
				result[i] = nil
				continue
			}
			entry, found := cmdCtx.Cache.RawGet(key)
			if !found {
				result[i] = nil
				continue
			}
			str, ok := entry.Value.(string)
			if !ok {
				result[i] = nil
				continue
			}
			result[i] = str
		}
		return result
	})
}

// handleMset sets multiple key-value pairs in a single call.
func (b *BaseEvaluator) handleMset(cmdCtx *CommandContext) Result {
	if len(cmdCtx.Args)%2 != 0 {
		return Result{Value: resp.ErrArgs("mset")}
	}
	return dispatch(cmdCtx, func() interface{} {
		for i := 0; i < len(cmdCtx.Args); i += 2 {
			if setErr := cmdCtx.Cache.RawSet(cmdCtx.Args[i], cmdCtx.Args[i+1], 0); setErr != nil {
				return setErr
			}
		}
		return "OK"
	})
}

// incrByDelta is shared logic for INCR, DECR, INCRBY, DECRBY.
// Must be called inside a dispatch closure (cache lock is held).
func incrByDelta(cmdCtx *CommandContext, key string, delta int64) interface{} {
	_, state := cmdCtx.Cache.TTLInternal(key)
	if state == cache.ValueExpired {
		cmdCtx.Cache.RawDelete(key)
	}

	current := int64(0)
	rawTTL := int64(0)
	if entry, found := cmdCtx.Cache.RawGet(key); found {
		str, ok := entry.Value.(string)
		if !ok {
			return resp.ErrWrongType
		}
		parsed, err := strconv.ParseInt(str, 10, 64)
		if err != nil {
			return resp.ErrNotInteger
		}
		current = parsed
		rawTTL = cmdCtx.Cache.RawTTL(key)
	}

	newVal := current + delta
	if setErr := cmdCtx.Cache.RawSet(key, strconv.FormatInt(newVal, 10), rawTTL); setErr != nil {
		return setErr
	}
	return newVal
}

// handleSet implements SET key value [NX|XX] [EX seconds|PX milliseconds] [KEEPTTL]
func (b *BaseEvaluator) handleSet(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	val := cmdCtx.Args[1]

	var (
		nx         bool
		xx         bool
		keepTTL    bool
		expiration int64
	)

	for i := 2; i < len(cmdCtx.Args); i++ {
		flag := strings.ToUpper(cmdCtx.Args[i])
		switch flag {
		case "NX":
			nx = true
		case "XX":
			xx = true
		case "KEEPTTL":
			keepTTL = true
		case "EX":
			if i+1 >= len(cmdCtx.Args) {
				return Result{Value: resp.ErrSyntax()}
			}
			i++
			secs, err := strconv.ParseInt(cmdCtx.Args[i], 10, 64)
			if err != nil || secs <= 0 {
				return Result{Value: resp.ErrSyntax()}
			}
			expiration = time.Now().Add(time.Duration(secs) * time.Second).UnixNano()
		case "PX":
			if i+1 >= len(cmdCtx.Args) {
				return Result{Value: resp.ErrSyntax()}
			}
			i++
			ms, err := strconv.ParseInt(cmdCtx.Args[i], 10, 64)
			if err != nil || ms <= 0 {
				return Result{Value: resp.ErrSyntax()}
			}
			expiration = time.Now().Add(time.Duration(ms) * time.Millisecond).UnixNano()
		default:
			return Result{Value: resp.ErrSyntax()}
		}
	}

	executeFn := func() interface{} {
		_, found := cmdCtx.Cache.RawGet(key)
		if nx && found {
			_, state := cmdCtx.Cache.TTLInternal(key)
			if state != cache.ValueExpired {
				return nil
			}
		}
		if xx && !found {
			return nil
		}

		exp := expiration
		if keepTTL {
			exp = cmdCtx.Cache.RawTTL(key)
		}

		if err := cmdCtx.Cache.RawSet(key, val, exp); err != nil {
			return err
		}
		return "OK"
	}
	return dispatch(cmdCtx, executeFn)
}

// handleSetnx implements SETNX key value.
// Returns 1 if set, 0 if key already exists (non-expired).
func (b *BaseEvaluator) handleSetnx(cmdCtx *CommandContext) Result {
	key, val := cmdCtx.Args[0], cmdCtx.Args[1]
	executeFn := func() interface{} {
		_, found := cmdCtx.Cache.RawGet(key)
		if found {
			_, state := cmdCtx.Cache.TTLInternal(key)
			if state != cache.ValueExpired {
				return 0
			}
			cmdCtx.Cache.RawDelete(key)
		}
		if err := cmdCtx.Cache.RawSet(key, val, 0); err != nil {
			return err
		}
		return 1
	}
	return dispatch(cmdCtx, executeFn)
}

// handlePexpire implements PEXPIRE key milliseconds.
func (b *BaseEvaluator) handlePexpire(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	ms, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil {
		return Result{Err: ErrInvalidDuration}
	}
	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		_, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(key)
			return 0
		}
		expiration := time.Now().Add(time.Duration(ms) * time.Millisecond).UnixNano()
		if err := cmdCtx.Cache.RawSet(key, entry.Value, expiration); err != nil {
			return err
		}
		return 1
	}
	return dispatch(cmdCtx, executeFn)
}

// handlePttl implements PTTL key.
// Returns remaining TTL in milliseconds, -1 if no TTL, -2 if absent/expired.
func (b *BaseEvaluator) handlePttl(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
		ttl, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueAbsent || state == cache.ValueExpired {
			if state == cache.ValueExpired {
				cmdCtx.Cache.RawDelete(key)
			}
			return int64(-2)
		} else if ttl == 0 {
			return int64(-1)
		}
		return ttl.Milliseconds()
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleGet(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		_, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(key)
			return nil
		}
		return entry.Value
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleDelete(cmdCtx *CommandContext) Result {
	executeFn := func() interface{} {
		count := 0
		for _, key := range cmdCtx.Args {
			_, found := cmdCtx.Cache.RawGet(key)
			if found {
				cmdCtx.Cache.RawDelete(key)
				count++
			}
		}
		return count
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleExists(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
		_, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		_, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(key)
			return 0
		}
		return 1
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleExpire(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	secs, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil || secs <= 0 {
		return Result{Err: ErrInvalidDuration}
	}
	ttl := time.Duration(secs) * time.Second
	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		_, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(key)
			return 0
		}

		var expiration int64
		if ttl > 0 {
			expiration = time.Now().Add(ttl).UnixNano()
		}
		if err := cmdCtx.Cache.RawSet(key, entry.Value, expiration); err != nil {
			return err
		}
		return 1
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleTtl(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
		ttl, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueAbsent || state == cache.ValueExpired {
			if state == cache.ValueExpired {
				cmdCtx.Cache.RawDelete(key)
			}
			return int64(-2)
		} else if ttl == 0 {
			return int64(-1)
		} else {
			return int64(ttl.Seconds())
		}
	}
	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleDbsize(cmdCtx *CommandContext) Result {
	executeFn := func() interface{} {
		return cmdCtx.Cache.Len()
	}
	return dispatch(cmdCtx, executeFn)
}

func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.2fG", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.2fM", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.2fK", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func (b *BaseEvaluator) handleInfo(cmdCtx *CommandContext) Result {
	section := ""
	if len(cmdCtx.Args) > 0 {
		section = strings.ToLower(cmdCtx.Args[0])
	}

	if section != "" && section != "memory" {
		return Result{Value: ""}
	}

	executeFn := func() interface{} {
		used := cmdCtx.Cache.UsedBytes()
		maxMem := cmdCtx.Cache.MaxBytes()
		policy := cmdCtx.Cache.EvictionPolicyString()
		keys := cmdCtx.Cache.Len()

		var sb strings.Builder
		sb.WriteString("# Memory\r\n")
		fmt.Fprintf(&sb, "used_memory:%d\r\n", used)
		fmt.Fprintf(&sb, "used_memory_human:%s\r\n", humanBytes(used))
		fmt.Fprintf(&sb, "maxmemory:%d\r\n", maxMem)
		fmt.Fprintf(&sb, "maxmemory_human:%s\r\n", humanBytes(maxMem))
		fmt.Fprintf(&sb, "maxmemory_policy:%s\r\n", policy)
		fmt.Fprintf(&sb, "keys:%d\r\n", keys)
		fmt.Fprintf(&sb, "eviction_policy:%s\r\n", policy)
		return sb.String()
	}

	return dispatch(cmdCtx, executeFn)
}

func (b *BaseEvaluator) handleHello(cmdCtx *CommandContext) Result {
	version, err := strconv.Atoi(cmdCtx.Args[0])
	if err != nil || (version != 2 && version != 3) {
		return Result{Value: resp.MarshalError("NOPROTO unsupported protocol version")}
	}

	cmdCtx.Client.ProtoVersion = version

	info := map[string]interface{}{
		"server":  "gocache",
		"version": "0.1.0",
		"proto":   version,
		"mode":    "standalone",
		"role":    "master",
	}

	return Result{Value: info}
}
