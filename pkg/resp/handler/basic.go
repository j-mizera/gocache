package handler

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// constantTimeStringCompare returns true iff a == b, using a constant-time
// algorithm that does not leak password length or content via timing.
func constantTimeStringCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ErrInvalidDuration is returned when a duration argument cannot be parsed.
var ErrInvalidDuration = errors.New("invalid duration")

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

// HandleFlushDB clears the entire cache (single-DB server, same as FLUSHALL).
func HandleFlushDB(cmdCtx *command.Context) command.Result {
	return command.Dispatch(cmdCtx, func() interface{} {
		cmdCtx.Cache.Clear(cmdCtx.Context())
		return "OK"
	})
}

// HandleFlushAll clears the entire cache.
func HandleFlushAll(cmdCtx *command.Context) command.Result {
	return command.Dispatch(cmdCtx, func() interface{} {
		cmdCtx.Cache.Clear(cmdCtx.Context())
		return "OK"
	})
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
	return command.Dispatch(cmdCtx, func() interface{} {
		return incrByDelta(cmdCtx, key, 1)
	})
}

// HandleDecr atomically decrements the integer value stored at key by 1.
func HandleDecr(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	return command.Dispatch(cmdCtx, func() interface{} {
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
	return command.Dispatch(cmdCtx, func() interface{} {
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
	return command.Dispatch(cmdCtx, func() interface{} {
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
	return command.Dispatch(cmdCtx, func() interface{} {
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
		if setErr := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, newStr, rawTTL); setErr != nil {
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
	return command.Dispatch(cmdCtx, func() interface{} {
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
		if setErr := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, newStr, rawTTL); setErr != nil {
			return setErr
		}
		return int64(len(newStr))
	})
}

// HandleStrlen returns the length of the string stored at key, or 0 if absent.
func HandleStrlen(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	return command.Dispatch(cmdCtx, func() interface{} {
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

// HandleMget returns the values for all specified keys (nil for absent/non-string).
func HandleMget(cmdCtx *command.Context) command.Result {
	keys := cmdCtx.Args
	return command.Dispatch(cmdCtx, func() interface{} {
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

// HandleMset sets multiple key-value pairs in a single call.
func HandleMset(cmdCtx *command.Context) command.Result {
	if len(cmdCtx.Args)%2 != 0 {
		return command.Result{Value: resp.ErrArgs("mset")}
	}
	return command.Dispatch(cmdCtx, func() interface{} {
		for i := 0; i < len(cmdCtx.Args); i += 2 {
			if setErr := cmdCtx.Cache.RawSet(cmdCtx.Context(), cmdCtx.Args[i], cmdCtx.Args[i+1], 0); setErr != nil {
				return setErr
			}
		}
		return "OK"
	})
}

// incrByDelta is shared logic for INCR, DECR, INCRBY, DECRBY.
// Must be called inside a Dispatch closure (cache lock is held).
func incrByDelta(cmdCtx *command.Context, key string, delta int64) interface{} {
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
	if setErr := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, strconv.FormatInt(newVal, 10), rawTTL); setErr != nil {
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

		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, val, exp); err != nil {
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
	executeFn := func() interface{} {
		_, found := cmdCtx.Cache.RawGet(key)
		if found {
			_, state := cmdCtx.Cache.TTLInternal(key)
			if state != cache.ValueExpired {
				return 0
			}
			cmdCtx.Cache.RawDelete(key)
		}
		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, val, 0); err != nil {
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
		return command.Result{Err: ErrInvalidDuration}
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
		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, entry.Value, expiration); err != nil {
			return err
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
	executeFn := func() interface{} {
		if _, found := cmdCtx.Cache.RawGet(key); !found {
			return int64(-2)
		}
		ttl, state := cmdCtx.Cache.TTLInternal(key)
		switch state {
		case cache.ValueExpired:
			cmdCtx.Cache.RawDelete(key)
			return int64(-2)
		case cache.ValueAbsent:
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
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleDelete implements DEL key [key ...].
func HandleDelete(cmdCtx *command.Context) command.Result {
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
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleExists implements EXISTS key.
func HandleExists(cmdCtx *command.Context) command.Result {
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
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleExpire implements EXPIRE key seconds.
func HandleExpire(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	secs, err := strconv.ParseInt(cmdCtx.Args[1], 10, 64)
	if err != nil || secs <= 0 {
		return command.Result{Err: ErrInvalidDuration}
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
		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, entry.Value, expiration); err != nil {
			return err
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
	executeFn := func() interface{} {
		// Distinguish "key missing" (-2) from "key present with no TTL" (-1).
		// TTLInternal returns ValueAbsent for both cases, so we must check
		// key existence explicitly first.
		if _, found := cmdCtx.Cache.RawGet(key); !found {
			return int64(-2)
		}
		ttl, state := cmdCtx.Cache.TTLInternal(key)
		switch state {
		case cache.ValueExpired:
			cmdCtx.Cache.RawDelete(key)
			return int64(-2)
		case cache.ValueAbsent:
			// Key exists but has no TTL set.
			return int64(-1)
		default:
			return int64(ttl.Seconds())
		}
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleDBSize implements DBSIZE.
func HandleDBSize(cmdCtx *command.Context) command.Result {
	executeFn := func() interface{} {
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
				return command.Result{Value: resp.MarshalError("ERR syntax error")}
			}
			// AUTH username password -- username is ignored for now (single-user)
			if cmdCtx.RequirePass != "" && !constantTimeStringCompare(args[2], cmdCtx.RequirePass) {
				return command.Result{Value: resp.MarshalError("WRONGPASS invalid password")}
			}
			cmdCtx.Client.Authenticated = true
			args = args[3:]
		case "SETNAME":
			if len(args) < 2 {
				return command.Result{Value: resp.MarshalError("ERR syntax error")}
			}
			// Client name is informational; not stored yet.
			args = args[2:]
		case "REXV":
			if len(args) < 2 {
				return command.Result{Value: resp.MarshalError("ERR syntax error")}
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
			return command.Result{Value: resp.MarshalError("ERR syntax error")}
		}
	}

	info := map[string]interface{}{
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
