package handler

import (
	"gocache/pkg/cache"
	"gocache/pkg/command"
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

// HandleSinter implements SINTER key [key ...]
func HandleSinter(cmdCtx *command.Context) command.Result {
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
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSunion implements SUNION key [key ...]
func HandleSunion(cmdCtx *command.Context) command.Result {
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
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSdiff implements SDIFF key [key ...]
func HandleSdiff(cmdCtx *command.Context) command.Result {
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
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSadd implements SADD key member [member ...]
func HandleSadd(cmdCtx *command.Context) command.Result {
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

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSrem implements SREM key member [member ...]
func HandleSrem(cmdCtx *command.Context) command.Result {
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

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSmembers implements SMEMBERS key
func HandleSmembers(cmdCtx *command.Context) command.Result {
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

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSismember implements SISMEMBER key member
func HandleSismember(cmdCtx *command.Context) command.Result {
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

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleScard implements SCARD key
func HandleScard(cmdCtx *command.Context) command.Result {
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

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSpop implements SPOP key
func HandleSpop(cmdCtx *command.Context) command.Result {
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

	return command.Dispatch(cmdCtx, executeFn)
}
