package handler

import (
	"crypto/subtle"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"sync"
	"time"

	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

var msetPerShardPool = sync.Pool{
	New: func() any {
		s := make([][]cache.BulkPair, 256)
		return &s
	},
}

// constantTimeStringCompare returns true iff a == b, using a constant-time
// algorithm that does not leak password length or content via timing.
func constantTimeStringCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// HandlePing returns PONG with no arguments, or echoes the first argument.
func HandlePing(cmdCtx *command.Context) command.Result {
	if len(cmdCtx.Args) == 0 {
		return command.Result{Value: "PONG"}
	}
	return command.Result{Value: cmdCtx.Args[0]}
}

// HandleEcho returns the single argument as-is.
func HandleEcho(cmdCtx *command.Context) command.Result {
	return command.Result{Value: cmdCtx.Args[0]}
}

// HandleSelect accepts only DB 0; any other index is an error.
func HandleSelect(cmdCtx *command.Context) command.Result {
	idx, err := strconv.Atoi(cmdCtx.Args[0])
	if err != nil || idx != 0 {
		return command.Result{Value: resp.MarshalError("ERR DB index is out of range")}
	}
	return command.Result{Value: "OK"}
}

// HandleFlushDB clears the entire cache (single-DB server, equivalent to FLUSHALL).
func HandleFlushDB(cmdCtx *command.Context) command.Result {
	return command.Dispatch(cmdCtx, func() any {
		cmdCtx.Cache.Clear(cmdCtx.Context())
		return "OK"
	})
}

// HandleFlushAll is a Redis-compatibility alias for FLUSHDB. Multi-DB is not
// supported, so there is nothing extra to flush.
func HandleFlushAll(cmdCtx *command.Context) command.Result {
	return HandleFlushDB(cmdCtx)
}

// HandleAuth validates a password against RequirePass.
func HandleAuth(cmdCtx *command.Context) command.Result {
	if cmdCtx.RequirePass == "" {
		return command.Result{Value: resp.MarshalError("ERR Client sent AUTH, but no password is set")}
	}
	if !constantTimeStringCompare(cmdCtx.Args[0], cmdCtx.RequirePass) {
		return command.Result{Value: resp.MarshalError("WRONGPASS invalid username-password pair")}
	}
	cmdCtx.Client.Authenticated = true
	return command.Result{Value: "OK"}
}

// HandleIncr atomically increments the integer value stored at key by 1.
func HandleIncr(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	return command.Dispatch(cmdCtx, func() any {
		return incrByDelta(cmdCtx, key, 1)
	})
}

// HandleDecr atomically decrements the integer value stored at key by 1.
func HandleDecr(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	return command.Dispatch(cmdCtx, func() any {
		return incrByDelta(cmdCtx, key, -1)
	})
}

// HandleIncrBy increments by the supplied integer delta.
func HandleIncrBy(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	delta, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil {
		return command.Result{Value: resp.ErrNotIntegerValue()}
	}
	return command.Dispatch(cmdCtx, func() any {
		return incrByDelta(cmdCtx, key, delta)
	})
}

// HandleDecrBy decrements by the supplied integer delta.
func HandleDecrBy(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	delta, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil {
		return command.Result{Value: resp.ErrNotIntegerValue()}
	}
	return command.Dispatch(cmdCtx, func() any {
		return incrByDelta(cmdCtx, key, -delta)
	})
}

// HandleIncrByFloat increments the float value stored at key.
func HandleIncrByFloat(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	incr, err := strconv.ParseFloat(cmdCtx.Args[1], 64)
	if err != nil {
		return command.Result{Value: resp.ErrNotFloatValue()}
	}
	return command.Dispatch(cmdCtx, func() any {
		lazyExpire(cmdCtx.Cache, key)

		existing := 0.0
		if entry, found := cmdCtx.Cache.RawGet(key); found {
			if entry.ValueType != cache.ObjTypeBytes {
				return resp.ErrWrongType
			}
			b := cmdCtx.Cache.ResolvePacked(entry)
			existing, err = strconv.ParseFloat(string(b), 64)
			if err != nil {
				return resp.ErrNotFloat
			}
		}

		newVal := existing + incr
		newStr := strconv.FormatFloat(newVal, 'f', -1, 64)
		rawTTL := cmdCtx.Cache.RawTTL(key)
		if setErr := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, []byte(newStr), rawTTL); setErr != nil {
			return setErr
		}
		return newStr
	})
}

// HandleAppend appends value to the string stored at key, creating it if absent.
// Returns the new length of the string.
func HandleAppend(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	suffix := cmdCtx.Args[1]
	return command.Dispatch(cmdCtx, func() any {
		lazyExpire(cmdCtx.Cache, key)

		var existing []byte
		rawTTL := int64(0)
		if entry, found := cmdCtx.Cache.RawGet(key); found {
			if entry.ValueType != cache.ObjTypeBytes {
				return resp.ErrWrongType
			}
			existing = cmdCtx.Cache.ResolvePacked(entry)
			rawTTL = cmdCtx.Cache.RawTTL(key)
		}

		newBytes := make([]byte, len(existing)+len(suffix))
		copy(newBytes, existing)
		copy(newBytes[len(existing):], suffix)
		if setErr := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, newBytes, rawTTL); setErr != nil {
			return setErr
		}
		return int64(len(newBytes))
	})
}

// HandleStrlen returns the length of the string stored at key, or 0 if absent.
func HandleStrlen(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	return command.Dispatch(cmdCtx, func() any {
		if lazyExpire(cmdCtx.Cache, key) {
			return int64(0)
		}

		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return int64(0)
		}
		if entry.ValueType != cache.ObjTypeBytes {
			return resp.ErrWrongType
		}
		return int64(len(cmdCtx.Cache.ResolvePacked(entry)))
	})
}

// HandleMget returns the values for all specified keys (nil for absent/non-string).
func HandleMget(cmdCtx *command.Context) command.Result {
	keys := cmdCtx.Args
	cmdCtx.TouchedShards = cmdCtx.Cache.TouchedShards(keys)
	return command.Dispatch(cmdCtx, func() any {
		result := make([]any, len(keys))
		for i, key := range keys {
			if lazyExpire(cmdCtx.Cache, key) {
				result[i] = nil
				continue
			}
			entry, found := cmdCtx.Cache.RawGet(key)
			if !found {
				result[i] = nil
				continue
			}
			if entry.ValueType != cache.ObjTypeBytes {
				result[i] = nil
				continue
			}
			// Copy out: the slice returned by ResolvePacked aliases the
			// slab region, which is not safe to hand off to the RESP
			// serializer when subsequent mutations may reuse the slot.
			src := cmdCtx.Cache.ResolvePacked(entry)
			buf := make([]byte, len(src))
			copy(buf, src)
			result[i] = buf
		}
		return result
	})
}

// HandleMset sets multiple key-value pairs in a single call.
//
// Hot-path optimization (#47): each MSET call groups its keys by their
// owning shard upfront, then dispatches one batched write per touched
// shard via Shard.BulkSetBytes. This keeps each shard's items map +
// slab arena state hot in L1/L2 across every key in that shard's
// batch. The pre-#47 path called Cache.RawSet per key, which re-fetched
// the same shard struct, slab state, and items map cache lines on
// every iteration — that cache-line churn was the dominant cost of
// pipelined MSET at N>1, not lock acquisition (see #43/#46 for the
// lock-cost finding).
func HandleMset(cmdCtx *command.Context) command.Result {
	if len(cmdCtx.Args)%2 != 0 {
		return command.Result{Value: resp.ErrArgs("mset")}
	}
	shardCount := cmdCtx.Cache.ShardCount()
	psp := msetPerShardPool.Get().(*[][]cache.BulkPair)
	perShard := *psp
	if len(perShard) < shardCount {
		perShard = make([][]cache.BulkPair, shardCount)
		*psp = perShard
	}
	defer func() {
		for i := range perShard {
			perShard[i] = perShard[i][:0]
		}
		msetPerShardPool.Put(psp)
	}()

	var mask [4]uint64
	for i := 0; i < len(cmdCtx.Args); i += 2 {
		idx := cmdCtx.Cache.ShardIndexOf(cmdCtx.Args[i])
		perShard[idx] = append(perShard[idx], cache.BulkPair{
			Key:   cmdCtx.Args[i],
			Value: []byte(cmdCtx.Args[i+1]),
		})
		mask[idx/64] |= 1 << uint(idx%64)
	}

	total := 0
	for i := range mask {
		total += bits.OnesCount64(mask[i])
	}
	touched := make([]int, 0, total)
	for i := range mask {
		w := mask[i]
		base := i * 64
		for w != 0 {
			bit := bits.TrailingZeros64(w)
			touched = append(touched, base+bit)
			w &= w - 1
		}
	}
	cmdCtx.TouchedShards = touched

	return command.Dispatch(cmdCtx, func() any {
		for shardID, pairs := range perShard {
			if len(pairs) == 0 {
				continue
			}
			shard := cmdCtx.Cache.ShardByIndex(shardID)
			if shard == nil {
				continue
			}
			if err := shard.BulkSetBytes(cmdCtx.Context(), pairs, 0); err != nil {
				return err
			}
		}
		return "OK"
	})
}

// incrByDelta is shared logic for INCR, DECR, INCRBY, DECRBY.
// Must be called inside a Dispatch closure (cache lock is held).
func incrByDelta(cmdCtx *command.Context, key string, delta int64) any {
	lazyExpire(cmdCtx.Cache, key)

	current := int64(0)
	rawTTL := int64(0)
	if entry, found := cmdCtx.Cache.RawGet(key); found {
		if entry.ValueType != cache.ObjTypeBytes {
			return resp.ErrWrongType
		}
		b := cmdCtx.Cache.ResolvePacked(entry)
		parsed, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return resp.ErrNotInteger
		}
		current = parsed
		rawTTL = cmdCtx.Cache.RawTTL(key)
	}

	newVal := current + delta
	newBytes := strconv.AppendInt(make([]byte, 0, 20), newVal, 10)
	if setErr := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, newBytes, rawTTL); setErr != nil {
		return setErr
	}
	return newVal
}

// HandleSet implements SET key value [NX|XX] [EX seconds|PX milliseconds] [KEEPTTL]
func HandleSet(cmdCtx *command.Context) command.Result {
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
				return command.Result{Value: resp.ErrSyntax()}
			}
			i++
			secs, err := strconv.ParseInt(cmdCtx.Args[i], 10, 64)
			if err != nil || secs <= 0 {
				return command.Result{Value: resp.ErrSyntax()}
			}
			expiration = time.Now().Add(time.Duration(secs) * time.Second).UnixNano()
		case "PX":
			if i+1 >= len(cmdCtx.Args) {
				return command.Result{Value: resp.ErrSyntax()}
			}
			i++
			ms, err := strconv.ParseInt(cmdCtx.Args[i], 10, 64)
			if err != nil || ms <= 0 {
				return command.Result{Value: resp.ErrSyntax()}
			}
			expiration = time.Now().Add(time.Duration(ms) * time.Millisecond).UnixNano()
		default:
			return command.Result{Value: resp.ErrSyntax()}
		}
	}

	executeFn := func() any {
		_, found := cmdCtx.Cache.RawGet(key)
		if nx && found {
			// Live (non-expired) key blocks NX; expired key is lazily deleted and SET proceeds.
			if !lazyExpire(cmdCtx.Cache, key) {
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

		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, []byte(val), exp); err != nil {
			return err
		}
		return "OK"
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSetnx implements SETNX key value.
// Returns 1 if set, 0 if key already exists (non-expired).
func HandleSetnx(cmdCtx *command.Context) command.Result {
	key, val := cmdCtx.Args[0], cmdCtx.Args[1]
	executeFn := func() any {
		_, found := cmdCtx.Cache.RawGet(key)
		if found {
			// Live (non-expired) key blocks SETNX; expired key is lazily deleted and SETNX proceeds.
			if !lazyExpire(cmdCtx.Cache, key) {
				return 0
			}
		}
		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, []byte(val), 0); err != nil {
			return err
		}
		return 1
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandlePexpire implements PEXPIRE key milliseconds.
func HandlePexpire(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	ms, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil {
		return command.Result{Err: resp.ErrInvalidExpireTime}
	}
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if lazyExpire(cmdCtx.Cache, key) {
			return 0
		}
		_ = entry
		expiration := time.Now().Add(time.Duration(ms) * time.Millisecond).UnixNano()
		if !cmdCtx.Cache.SetExpiration(key, expiration) {
			return 0
		}
		return 1
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandlePttl implements PTTL key.
// Returns remaining TTL in milliseconds, -1 if the key exists but has no TTL,
// -2 if the key does not exist or has expired.
func HandlePttl(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		ttl, state := cmdCtx.Cache.TTLInternal(key)
		switch state {
		case cache.ValueExpired:
			cmdCtx.Cache.RawDelete(key)
			return int64(-2)
		case cache.ValueAbsent:
			return int64(-2)
		case cache.ValueNoExpire:
			return int64(-1)
		default:
			return ttl.Milliseconds()
		}
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleGet implements GET key.
func HandleGet(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if lazyExpire(cmdCtx.Cache, key) {
			return nil
		}
		if entry.ValueType != cache.ObjTypeBytes {
			return resp.ErrWrongType
		}
		// Copy out — the returned slice must not alias the slab slot
		// once the cache lock is released.
		src := cmdCtx.Cache.ResolvePacked(entry)
		buf := make([]byte, len(src))
		copy(buf, src)
		return buf
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleDelete implements DEL key [key ...].
func HandleDelete(cmdCtx *command.Context) command.Result {
	cmdCtx.TouchedShards = cmdCtx.Cache.TouchedShards(cmdCtx.Args)
	executeFn := func() any {
		count := 0
		for _, key := range cmdCtx.Args {
			_, found := cmdCtx.Cache.RawGet(key)
			if found {
				if lazyExpire(cmdCtx.Cache, key) {
					continue
				}
				cmdCtx.Cache.RawDelete(key)
				count++
			}
		}
		return count
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleExists implements EXISTS key.
func HandleExists(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		_, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if lazyExpire(cmdCtx.Cache, key) {
			return 0
		}
		return 1
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleExpire implements EXPIRE key seconds.
func HandleExpire(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	secs, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil || secs < 0 {
		return command.Result{Err: resp.ErrInvalidExpireTime}
	}
	executeFn := func() any {
		_, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if lazyExpire(cmdCtx.Cache, key) {
			return 0
		}
		if secs == 0 {
			cmdCtx.Cache.RawDelete(key)
			return 1
		}
		expiration := time.Now().Add(time.Duration(secs) * time.Second).UnixNano()
		if !cmdCtx.Cache.SetExpiration(key, expiration) {
			return 0
		}
		return 1
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleTtl implements TTL key.
// Returns remaining TTL in seconds, -1 if the key exists but has no TTL,
// -2 if the key does not exist or has expired.
func HandleTtl(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	executeFn := func() any {
		ttl, state := cmdCtx.Cache.TTLInternal(key)
		switch state {
		case cache.ValueExpired:
			cmdCtx.Cache.RawDelete(key)
			return int64(-2)
		case cache.ValueAbsent:
			return int64(-2)
		case cache.ValueNoExpire:
			return int64(-1)
		default:
			return int64(ttl.Seconds())
		}
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleDBSize implements DBSIZE.
func HandleDBSize(cmdCtx *command.Context) command.Result {
	executeFn := func() any {
		return cmdCtx.Cache.Len()
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// humanBytes formats a byte count into a human-readable string.
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

// HandleInfo implements INFO [section].
func HandleInfo(cmdCtx *command.Context) command.Result {
	section := ""
	if len(cmdCtx.Args) > 0 {
		section = strings.ToLower(cmdCtx.Args[0])
	}

	if section != "" && section != "memory" {
		return command.Result{Value: ""}
	}

	executeFn := func() any {
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

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHello implements the HELLO command for protocol negotiation.
func HandleHello(cmdCtx *command.Context) command.Result {
	version, err := strconv.Atoi(cmdCtx.Args[0])
	if err != nil || (version != 2 && version != 3) {
		return command.Result{Value: resp.MarshalError("NOPROTO unsupported protocol version")}
	}

	cmdCtx.Client.ProtoVersion = version

	// Parse optional keyword-value pairs: AUTH user pass, SETNAME name, REXV version
	args := cmdCtx.Args[1:]
	for len(args) > 0 {
		keyword := strings.ToUpper(args[0])
		switch keyword {
		case "AUTH":
			if len(args) < 3 {
				return command.Result{Value: resp.ErrSyntax()}
			}
			// AUTH username password -- username is ignored for now (single-user)
			if cmdCtx.RequirePass != "" && !constantTimeStringCompare(args[2], cmdCtx.RequirePass) {
				return command.Result{Value: resp.MarshalError("WRONGPASS invalid password")}
			}
			cmdCtx.Client.Authenticated = true
			args = args[3:]
		case "SETNAME":
			if len(args) < 2 {
				return command.Result{Value: resp.ErrSyntax()}
			}
			// Client name is informational; not stored yet.
			args = args[2:]
		case "REXV":
			if len(args) < 2 {
				return command.Result{Value: resp.ErrSyntax()}
			}
			rv, err := strconv.Atoi(args[1])
			if err != nil || rv < 0 {
				return command.Result{Value: resp.MarshalError("ERR invalid REXV version")}
			}
			if rv > 1 {
				return command.Result{Value: resp.MarshalError("ERR unsupported REXV version")}
			}
			cmdCtx.Client.RexVersion = rv
			args = args[2:]
		default:
			return command.Result{Value: resp.ErrSyntax()}
		}
	}

	info := map[string]any{
		"server":  "gocache",
		"version": "0.1.0",
		"proto":   version,
		"mode":    "standalone",
		"role":    "master",
	}
	if cmdCtx.Client.RexVersion > 0 {
		info["rexv"] = cmdCtx.Client.RexVersion
	}

	return command.Result{Value: info}
}
