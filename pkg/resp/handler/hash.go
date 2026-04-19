package handler

import (
	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// HandleHset implements HSET key field value [field value ...]
func HandleHset(cmdCtx *command.Context) command.Result {
	if (len(cmdCtx.Args)-1)%2 != 0 {
		return command.Result{Value: resp.ErrArgs("hset")}
	}

	key := cmdCtx.Args[0]
	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		var hash map[string]string
		added := 0

		if !found {
			hash = make(map[string]string)
		} else {
			if entry.ValueType != cache.ObjTypeHash {
				return resp.ErrWrongType
			}
			hash = entry.Value.(map[string]string)
		}

		for i := 1; i < len(cmdCtx.Args); i += 2 {
			field := cmdCtx.Args[i]
			value := cmdCtx.Args[i+1]
			if _, exists := hash[field]; !exists {
				added++
			}
			hash[field] = value
		}

		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, hash, 0); err != nil {
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
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}

		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
		}

		hash := entry.Value.(map[string]string)
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
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}

		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
		}

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
			if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, hash, 0); err != nil {
				return err
			}
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
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}

		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
		}

		hash := entry.Value.(map[string]string)
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
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return map[string]string{}
		}

		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
		}

		return entry.Value.(map[string]string)
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

		hash := entry.Value.(map[string]string)
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
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []any{}
		}

		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
		}

		hash := entry.Value.(map[string]string)
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

		hash := entry.Value.(map[string]string)
		return len(hash)
	}

	return command.Dispatch(cmdCtx, executeFn)
}
