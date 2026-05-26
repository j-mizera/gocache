package handler

import (
	apicommand "gocache/api/command"
	"gocache/pkg/cache"
	"gocache/pkg/cache/packed"
	"gocache/pkg/command"
	"gocache/commons/resp"
)

// Hash commands operate on two physical encodings:
//
//   EncPacked: cmdCtx.Cache.ResolvePacked(entry) — a packed.Hash* buffer. Mutations go
//             through the packed package (in-place splice, same-size
//             replacements are alloc-free).
//   EncNative: entry.Value.(map[string]string) — Go-map path. Used for
//             large hashes or hashes promoted past the threshold.
//
// Handlers that read (HGET, HEXISTS, HGETALL, HKEYS, HVALS, HLEN) switch on
// Encoding. Handlers that write (HSET, HDEL) additionally check whether the
// post-op state should promote Packed→Native and store back accordingly.
// No demotion — matches Valkey semantics.

// HandleHset implements HSET key field value [field value ...]
func HandleHset(cmdCtx *command.Context) apicommand.Result {
	if (len(cmdCtx.Args)-1)%2 != 0 {
		return apicommand.Result{Value: resp.ErrArgs("hset")}
	}

	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)

		if !found {
			// Fresh hash: start Packed unless the first value already
			// exceeds maxValue (then skip straight to Native).
			return hsetStartPacked(cmdCtx, key, cmdCtx.Args[1:])
		}
		if entry.ValueType != cache.ObjTypeHash {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			return hsetPacked(cmdCtx, key, cmdCtx.Cache.ResolvePacked(entry), cmdCtx.Args[1:])
		default:
			return hsetNative(cmdCtx, key, entry.Value.(map[string]string), cmdCtx.Args[1:])
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}


// hsetStartPacked seeds a new hash from a Packed buffer. Same promotion
// logic as hsetPacked so a single field larger than maxValue skips Packed
// entirely.
func hsetStartPacked(cmdCtx *command.Context, key string, kvs []string) any {
	return hsetPacked(cmdCtx, key, packed.HashNew(), kvs)
}

func hsetPacked(cmdCtx *command.Context, key string, buf []byte, kvs []string) any {
	t := cmdCtx.Cache.PackedThresholds()
	ttl := cmdCtx.Cache.RawTTL(key)
	newBuf, added, promoted, err := packed.HashSetBatch(buf, kvs, t.HashMaxEntries, t.HashMaxValue)
	if err != nil {
		return err
	}
	if promoted {
		m, perr := packed.HashToMap(newBuf)
		if perr != nil {
			return perr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, m, hashMapSize(m), ttl); err != nil {
			return err
		}
		return added
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeHash, newBuf, ttl); err != nil {
		return err
	}
	return added
}

// hsetNative mutates an existing Native hash. Reads the cached NativeSize
// and tracks deltas as fields are added or replaced — never walks the map.
func hsetNative(cmdCtx *command.Context, key string, hash map[string]string, kvs []string) any {
	n, err := finishHsetNative(cmdCtx, key, hash, cmdCtx.Cache.NativeSize(key), kvs)
	if err != nil {
		return err
	}
	return n
}

// finishHsetNative applies kvs to hash and persists with the resulting
// payload byte size. priorSize is the byte cost of the existing map
// (excluding key + per-entry overhead). Returns the number of newly-added
// fields (HSET return semantics).
func finishHsetNative(cmdCtx *command.Context, key string, hash map[string]string, priorSize int64, kvs []string) (int, error) {
	added := 0
	size := priorSize
	for i := 0; i < len(kvs); i += 2 {
		field, value := kvs[i], kvs[i+1]
		if oldVal, exists := hash[field]; exists {
			size += int64(len(value)) - int64(len(oldVal))
		} else {
			added++
			size += int64(len(field)) + int64(len(value)) + cache.HashFieldOverhead
		}
		hash[field] = value
	}
	ttl := cmdCtx.Cache.RawTTL(key)
	if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, hash, size, ttl); err != nil {
		return 0, err
	}
	return added, nil
}

// hashMapSize returns the estimateSize byte cost for a map[string]string,
// excluding the per-entry overhead and key length (those live elsewhere
// in the chargedSize formula). Walks the map exactly once — used at the
// packed→native promotion boundary so we never re-walk on subsequent
// HSET calls.
func hashMapSize(hash map[string]string) int64 {
	var size int64
	for k, v := range hash {
		size += int64(len(k)) + int64(len(v)) + cache.HashFieldOverhead
	}
	return size
}

// HandleHget implements HGET key field
func HandleHget(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	field := cmdCtx.Args[1]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeHash {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			v, ok, err := packed.HashGet(cmdCtx.Cache.ResolvePacked(entry), field)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			// Copy out: the zero-copy slice must not outlive the dispatch.
			return string(v)
		default:
			hash := entry.Value.(map[string]string)
			if v, ok := hash[field]; ok {
				return v
			}
			return nil
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHdel implements HDEL key field [field ...]
func HandleHdel(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	fields := cmdCtx.Args[1:]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeHash {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			buf := cmdCtx.Cache.ResolvePacked(entry)
			deleted := 0
			for _, field := range fields {
				var removed bool
				var err error
				buf, removed, err = packed.HashDelete(buf, field)
				if err != nil {
					return err
				}
				if removed {
					deleted++
				}
			}
			n, _ := packed.HashLen(buf)
			if n == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				ttl := cmdCtx.Cache.RawTTL(key)
				if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeHash, buf, ttl); err != nil {
					return err
				}
			}
			return deleted
		default:
			hash := entry.Value.(map[string]string)
			size := cmdCtx.Cache.NativeSize(key)
			deleted := 0
			for _, field := range fields {
				if oldVal, exists := hash[field]; exists {
					size -= int64(len(field)) + int64(len(oldVal)) + cache.HashFieldOverhead
					delete(hash, field)
					deleted++
				}
			}
			if len(hash) == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				ttl := cmdCtx.Cache.RawTTL(key)
				if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, hash, size, ttl); err != nil {
					return err
				}
			}
			return deleted
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHexists implements HEXISTS key field
func HandleHexists(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	field := cmdCtx.Args[1]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeHash {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			ok, err := packed.HashContains(cmdCtx.Cache.ResolvePacked(entry), field)
			if err != nil {
				return err
			}
			if ok {
				return 1
			}
			return 0
		default:
			hash := entry.Value.(map[string]string)
			if _, exists := hash[field]; exists {
				return 1
			}
			return 0
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHgetall implements HGETALL key
func HandleHgetall(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return map[string]string{}
		}
		if entry.ValueType != cache.ObjTypeHash {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			// Materialize out-of-cache so the returned map owns its strings.
			m, err := packed.HashToMap(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			return m
		default:
			return entry.Value.(map[string]string)
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHkeys implements HKEYS key
func HandleHkeys(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []any{}
		}
		if entry.ValueType != cache.ObjTypeHash {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			fields, err := packed.HashFields(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			out := make([]any, 0, len(fields))
			for _, f := range fields {
				out = append(out, f)
			}
			return out
		default:
			hash := entry.Value.(map[string]string)
			out := make([]any, 0, len(hash))
			for field := range hash {
				out = append(out, field)
			}
			return out
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHvals implements HVALS key
func HandleHvals(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []any{}
		}
		if entry.ValueType != cache.ObjTypeHash {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			values, err := packed.HashValues(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			out := make([]any, 0, len(values))
			for _, v := range values {
				out = append(out, v)
			}
			return out
		default:
			hash := entry.Value.(map[string]string)
			out := make([]any, 0, len(hash))
			for _, v := range hash {
				out = append(out, v)
			}
			return out
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHlen implements HLEN key
func HandleHlen(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeHash {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			n, err := packed.HashLen(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			return n
		default:
			return len(entry.Value.(map[string]string))
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}
