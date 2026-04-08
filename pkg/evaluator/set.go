package evaluator

import (
	"gocache/pkg/cache"
	"gocache/pkg/resp"
)

// getSet retrieves the set stored at key. Returns nil (not an error) for a
// missing key (empty set semantics). Returns resp.ErrWrongType if the key
// holds a different data type.
func getSet(c *cache.Cache, key string) (map[string]struct{}, error) {
	entry, found := c.RawGet(key)
	if !found {
		return nil, nil
	}
	if entry.ValueType != cache.ObjTypeSet {
		return nil, resp.ErrWrongType
	}
	return entry.Value.(map[string]struct{}), nil
}

// SINTER key [key ...]
func (b *BaseEvaluator) handleSinter(cmdCtx *CommandContext) Result {
	keys := cmdCtx.Args
	executeFn := func() interface{} {
		first, err := getSet(cmdCtx.Cache, keys[0])
		if err != nil {
			return err
		}
		// Copy first set so we can mutate it.
		intersection := make(map[string]struct{}, len(first))
		for m := range first {
			intersection[m] = struct{}{}
		}

		for _, key := range keys[1:] {
			s, err := getSet(cmdCtx.Cache, key)
			if err != nil {
				return err
			}
			// Remove members not present in s.
			for m := range intersection {
				if _, ok := s[m]; !ok {
					delete(intersection, m)
				}
			}
		}

		result := make([]interface{}, 0, len(intersection))
		for m := range intersection {
			result = append(result, m)
		}
		return result
	}
	return dispatch(cmdCtx, executeFn)
}

// SUNION key [key ...]
func (b *BaseEvaluator) handleSunion(cmdCtx *CommandContext) Result {
	keys := cmdCtx.Args
	executeFn := func() interface{} {
		union := make(map[string]struct{})
		for _, key := range keys {
			s, err := getSet(cmdCtx.Cache, key)
			if err != nil {
				return err
			}
			for m := range s {
				union[m] = struct{}{}
			}
		}
		result := make([]interface{}, 0, len(union))
		for m := range union {
			result = append(result, m)
		}
		return result
	}
	return dispatch(cmdCtx, executeFn)
}

// SDIFF key [key ...]
func (b *BaseEvaluator) handleSdiff(cmdCtx *CommandContext) Result {
	keys := cmdCtx.Args
	executeFn := func() interface{} {
		first, err := getSet(cmdCtx.Cache, keys[0])
		if err != nil {
			return err
		}
		// Copy first set.
		diff := make(map[string]struct{}, len(first))
		for m := range first {
			diff[m] = struct{}{}
		}

		for _, key := range keys[1:] {
			s, err := getSet(cmdCtx.Cache, key)
			if err != nil {
				return err
			}
			for m := range s {
				delete(diff, m)
			}
		}

		result := make([]interface{}, 0, len(diff))
		for m := range diff {
			result = append(result, m)
		}
		return result
	}
	return dispatch(cmdCtx, executeFn)
}

// SADD key member [member ...]
func (b *BaseEvaluator) handleSadd(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	members := cmdCtx.Args[1:]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		var set map[string]struct{}
		added := 0

		if !found {
			set = make(map[string]struct{})
		} else {
			if entry.ValueType != cache.ObjTypeSet {
				return resp.ErrWrongType
			}
			set = entry.Value.(map[string]struct{})
		}

		for _, member := range members {
			if _, exists := set[member]; !exists {
				set[member] = struct{}{}
				added++
			}
		}

		if err := cmdCtx.Cache.RawSet(key, set, 0); err != nil {
			return err
		}
		return added
	}

	return dispatch(cmdCtx, executeFn)
}

// SREM key member [member ...]
func (b *BaseEvaluator) handleSrem(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	members := cmdCtx.Args[1:]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}

		if entry.ValueType != cache.ObjTypeSet {
			return resp.ErrWrongType
		}

		set := entry.Value.(map[string]struct{})
		removed := 0

		for _, member := range members {
			if _, exists := set[member]; exists {
				delete(set, member)
				removed++
			}
		}

		if len(set) == 0 {
			cmdCtx.Cache.RawDelete(key)
		} else {
			if err := cmdCtx.Cache.RawSet(key, set, 0); err != nil {
				return err
			}
		}

		return removed
	}

	return dispatch(cmdCtx, executeFn)
}

// SMEMBERS key
func (b *BaseEvaluator) handleSmembers(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []interface{}{}
		}

		if entry.ValueType != cache.ObjTypeSet {
			return resp.ErrWrongType
		}

		set := entry.Value.(map[string]struct{})
		result := make([]interface{}, 0, len(set))

		for member := range set {
			result = append(result, member)
		}

		return result
	}

	return dispatch(cmdCtx, executeFn)
}

// SISMEMBER key member
func (b *BaseEvaluator) handleSismember(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]
	member := cmdCtx.Args[1]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}

		if entry.ValueType != cache.ObjTypeSet {
			return resp.ErrWrongType
		}

		set := entry.Value.(map[string]struct{})
		if _, exists := set[member]; exists {
			return 1
		}
		return 0
	}

	return dispatch(cmdCtx, executeFn)
}

// SCARD key
func (b *BaseEvaluator) handleScard(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}

		if entry.ValueType != cache.ObjTypeSet {
			return resp.ErrWrongType
		}

		set := entry.Value.(map[string]struct{})
		return len(set)
	}

	return dispatch(cmdCtx, executeFn)
}

// SPOP key
func (b *BaseEvaluator) handleSpop(cmdCtx *CommandContext) Result {
	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}

		if entry.ValueType != cache.ObjTypeSet {
			return resp.ErrWrongType
		}

		set := entry.Value.(map[string]struct{})
		if len(set) == 0 {
			return nil
		}

		var popped string
		for member := range set {
			popped = member
			break
		}

		delete(set, popped)

		if len(set) == 0 {
			cmdCtx.Cache.RawDelete(key)
		} else {
			if err := cmdCtx.Cache.RawSet(key, set, 0); err != nil {
				return err
			}
		}

		return popped
	}

	return dispatch(cmdCtx, executeFn)
}
