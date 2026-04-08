package evaluator

import (
	"gocache/pkg/cache"
	"gocache/pkg/resp"
)

// HSET key field value [field value ...]
func (b *BaseEvaluator) handleHset(cmdCtx *CommandContext) Result {
	if (len(cmdCtx.Args)-1)%2 != 0 {
		return Result{Value: resp.ErrArgs("hset")}
	}

	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
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

		if err := cmdCtx.Cache.RawSet(key, hash, 0); err != nil {
			return err
		}
		return added
	}

	return dispatch(cmdCtx, executeFn)
}

// HGET key field
func (b *BaseEvaluator) handleHget(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	field := cmdCtx.Args[1]

	executeFn := func() interface{} {
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

	return dispatch(cmdCtx, executeFn)
}

// HDEL key field [field ...]
func (b *BaseEvaluator) handleHdel(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	fields := cmdCtx.Args[1:]

	executeFn := func() interface{} {
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
			if err := cmdCtx.Cache.RawSet(key, hash, 0); err != nil {
				return err
			}
		}

		return deleted
	}

	return dispatch(cmdCtx, executeFn)
}

// HEXISTS key field
func (b *BaseEvaluator) handleHexists(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	field := cmdCtx.Args[1]

	executeFn := func() interface{} {
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

	return dispatch(cmdCtx, executeFn)
}

// HGETALL key
func (b *BaseEvaluator) handleHgetall(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return map[string]string{}
		}

		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
		}

		return entry.Value.(map[string]string)
	}

	return dispatch(cmdCtx, executeFn)
}

// HKEYS key
func (b *BaseEvaluator) handleHkeys(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []interface{}{}
		}

		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
		}

		hash := entry.Value.(map[string]string)
		result := make([]interface{}, 0, len(hash))

		for field := range hash {
			result = append(result, field)
		}

		return result
	}

	return dispatch(cmdCtx, executeFn)
}

// HVALS key
func (b *BaseEvaluator) handleHvals(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []interface{}{}
		}

		if entry.ValueType != cache.ObjTypeHash {
			return resp.ErrWrongType
		}

		hash := entry.Value.(map[string]string)
		result := make([]interface{}, 0, len(hash))

		for _, value := range hash {
			result = append(result, value)
		}

		return result
	}

	return dispatch(cmdCtx, executeFn)
}

// HLEN key
func (b *BaseEvaluator) handleHlen(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
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

	return dispatch(cmdCtx, executeFn)
}
