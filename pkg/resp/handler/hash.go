package handler

import (
	"gocache/pkg/cache"
	"gocache/pkg/cache/packed"
	"gocache/pkg/command"
	"gocache/pkg/resp"
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
func HandleHset(cmdCtx *command.Context) command.Result {
	if (len(cmdCtx.Args)-1)%2 != 0 {
		return command.Result{Value: resp.ErrArgs("hset")}
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
			return resp.ErrWrongType
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
	buf := packed.HashNew()
	t := cmdCtx.Cache.PackedThresholds()
	added := 0
	for i := 0; i < len(kvs); i += 2 {
		var addedOne, promoted bool
		var err error
		buf, addedOne, promoted, err = packed.HashSet(buf, kvs[i], kvs[i+1], t.HashMaxEntries, t.HashMaxValue)
		if err != nil {
			return err
		}
		if addedOne {
			added++
		}
		if promoted {
			m, perr := packed.HashToMap(buf)
			if perr != nil {
				return perr
			}
			added += finishHsetNative(cmdCtx, key, m, kvs[i+2:])
			return added
		}
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeHash, buf, 0); err != nil {
		return err
	}
	return added
}

// hsetPacked mutates an existing Packed hash. On promotion it hands off to
// the native path with the remaining arguments.
func hsetPacked(cmdCtx *command.Context, key string, buf []byte, kvs []string) any {
	t := cmdCtx.Cache.PackedThresholds()
	added := 0
	for i := 0; i < len(kvs); i += 2 {
		var addedOne, promoted bool
		var err error
		buf, addedOne, promoted, err = packed.HashSet(buf, kvs[i], kvs[i+1], t.HashMaxEntries, t.HashMaxValue)
		if err != nil {
			return err
		}
		if addedOne {
			added++
		}
		if promoted {
			m, perr := packed.HashToMap(buf)
			if perr != nil {
				return perr
			}
			added += finishHsetNative(cmdCtx, key, m, kvs[i+2:])
			return added
		}
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeHash, buf, 0); err != nil {
		return err
	}
	return added
}

// hsetNative mutates an existing Native hash (or a freshly-promoted one).
func hsetNative(cmdCtx *command.Context, key string, hash map[string]string, kvs []string) any {
	added := finishHsetNative(cmdCtx, key, hash, kvs)
	return added
}

// finishHsetNative applies kvs to hash and persists. Returns the number of
// newly-added fields (HSET return semantics).
func finishHsetNative(cmdCtx *command.Context, key string, hash map[string]string, kvs []string) int {
	added := 0
	for i := 0; i < len(kvs); i += 2 {
		if _, exists := hash[kvs[i]]; !exists {
			added++
		}
		hash[kvs[i]] = kvs[i+1]
	}
	_ = cmdCtx.Cache.RawSet(cmdCtx.Context(), key, hash, 0)
	return added
}

// HandleHget implements HGET key field
func HandleHget(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	field := cmdCtx.Args[1]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
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
func HandleHdel(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	fields := cmdCtx.Args[1:]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
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
				if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeHash, buf, 0); err != nil {
					return err
				}
			}
			return deleted
		default:
			hash := entry.Value.(map[string]string)
			deleted := 0
			for _, field := range fields {
				if _, exists := hash[field]; exists {
					delete(hash, field)
					deleted++
				}
			}
			if len(hash) == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				_ = cmdCtx.Cache.RawSet(cmdCtx.Context(), key, hash, 0)
			}
			return deleted
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHexists implements HEXISTS key field
func HandleHexists(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	field := cmdCtx.Args[1]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
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
func HandleHgetall(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return map[string]string{}
		}
		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
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
func HandleHkeys(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []any{}
		}
		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
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
func HandleHvals(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []any{}
		}
		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
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
func HandleHlen(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
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
