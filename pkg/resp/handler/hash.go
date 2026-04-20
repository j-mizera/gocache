package handler

import (
	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// readHash decodes the byte-encoded hash at key, returning (map, found,
// wrongType). See readList in lists.go for the same pattern applied to
// lists.
func readHash(cacheInst *cache.Cache, key string) (map[string]string, bool, bool) {
	entry, ok := cacheInst.RawGet(key)
	if !ok {
		return nil, false, false
	}
	if entry.ValueType != cache.ObjTypeHash {
		return nil, true, true
	}
	b := entry.Value
	decoded, err := cache.DecodeHash(b)
	if err != nil {
		return nil, true, true
	}
	return decoded, true, false
}

// writeHash encodes h and stores it under key. An empty hash deletes the
// key entirely, matching the existing semantics.
func writeHash(cmdCtx *command.Context, key string, h map[string]string) error {
	if len(h) == 0 {
		cmdCtx.Cache.RawDelete(key)
		return nil
	}
	encoded, err := cache.EncodeHash(h)
	if err != nil {
		return err
	}
	return cmdCtx.Cache.RawSetTyped(cmdCtx.Context(), key, cache.ObjTypeHash, encoded, 0)
}

// HandleHset implements HSET key field value [field value ...]
func HandleHset(cmdCtx *command.Context) command.Result {
	if (len(cmdCtx.Args)-1)%2 != 0 {
		return command.Result{Value: resp.ErrArgs("hset")}
	}

	key := cmdCtx.Args[0]
	executeFn := func() any {
		hash, _, wrongType := readHash(cmdCtx.Cache, key)
		if wrongType {
			return resp.ErrWrongType
		}
		if hash == nil {
			hash = make(map[string]string)
		}

		added := 0
		for i := 1; i < len(cmdCtx.Args); i += 2 {
			field := cmdCtx.Args[i]
			value := cmdCtx.Args[i+1]
			if _, exists := hash[field]; !exists {
				added++
			}
			hash[field] = value
		}

		if err := writeHash(cmdCtx, key, hash); err != nil {
			return err
		}
		return added
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHget implements HGET key field
func HandleHget(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	field := cmdCtx.Args[1]

	executeFn := func() any {
		hash, found, wrongType := readHash(cmdCtx.Cache, key)
		if !found {
			return nil
		}
		if wrongType {
			return resp.ErrWrongType
		}
		if value, ok := hash[field]; ok {
			return value
		}
		return nil
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHdel implements HDEL key field [field ...]
func HandleHdel(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	fields := cmdCtx.Args[1:]

	executeFn := func() any {
		hash, found, wrongType := readHash(cmdCtx.Cache, key)
		if !found {
			return 0
		}
		if wrongType {
			return resp.ErrWrongType
		}

		deleted := 0
		for _, field := range fields {
			if _, exists := hash[field]; exists {
				delete(hash, field)
				deleted++
			}
		}

		if err := writeHash(cmdCtx, key, hash); err != nil {
			return err
		}
		return deleted
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHexists implements HEXISTS key field
func HandleHexists(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	field := cmdCtx.Args[1]

	executeFn := func() any {
		hash, found, wrongType := readHash(cmdCtx.Cache, key)
		if !found {
			return 0
		}
		if wrongType {
			return resp.ErrWrongType
		}
		if _, exists := hash[field]; exists {
			return 1
		}
		return 0
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHgetall implements HGETALL key
func HandleHgetall(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		hash, found, wrongType := readHash(cmdCtx.Cache, key)
		if !found {
			return map[string]string{}
		}
		if wrongType {
			return resp.ErrWrongType
		}
		return hash
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHkeys implements HKEYS key
func HandleHkeys(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		hash, found, wrongType := readHash(cmdCtx.Cache, key)
		if !found {
			return []any{}
		}
		if wrongType {
			return resp.ErrWrongType
		}
		result := make([]any, 0, len(hash))
		for field := range hash {
			result = append(result, field)
		}
		return result
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleHvals implements HVALS key
func HandleHvals(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		hash, found, wrongType := readHash(cmdCtx.Cache, key)
		if !found {
			return []any{}
		}
		if wrongType {
			return resp.ErrWrongType
		}
		result := make([]any, 0, len(hash))
		for _, value := range hash {
			result = append(result, value)
		}
		return result
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
		// O(1) — just reads the count prefix.
		n, err := cache.HashLen(entry.Value)
		if err != nil {
			return resp.ErrWrongType
		}
		return n
	}

	return command.Dispatch(cmdCtx, executeFn)
}
