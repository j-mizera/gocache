package handler

import (
	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// readSet decodes the byte-encoded set at key, returning (map, found,
// wrongType). Same contract as readList/readHash.
func readSet(c *cache.Cache, key string) (map[string]struct{}, bool, bool) {
	entry, ok := c.RawGet(key)
	if !ok {
		return nil, false, false
	}
	if entry.ValueType != cache.ObjTypeSet {
		return nil, true, true
	}
	b := entry.Value
	decoded, err := cache.DecodeSet(b)
	if err != nil {
		return nil, true, true
	}
	return decoded, true, false
}

// writeSet encodes s and stores it under key. An empty set deletes the key.
func writeSet(cmdCtx *command.Context, key string, s map[string]struct{}) error {
	if len(s) == 0 {
		cmdCtx.Cache.RawDelete(key)
		return nil
	}
	encoded, err := cache.EncodeSet(s)
	if err != nil {
		return err
	}
	return cmdCtx.Cache.RawSetTyped(cmdCtx.Context(), key, cache.ObjTypeSet, encoded, 0)
}

// HandleSinter implements SINTER key [key ...]
func HandleSinter(cmdCtx *command.Context) command.Result {
	keys := cmdCtx.Args
	executeFn := func() any {
		first, _, wrongType := readSet(cmdCtx.Cache, keys[0])
		if wrongType {
			return resp.ErrWrongType
		}
		intersection := make(map[string]struct{}, len(first))
		for m := range first {
			intersection[m] = struct{}{}
		}

		for _, key := range keys[1:] {
			s, _, wrongType := readSet(cmdCtx.Cache, key)
			if wrongType {
				return resp.ErrWrongType
			}
			for m := range intersection {
				if _, ok := s[m]; !ok {
					delete(intersection, m)
				}
			}
		}

		result := make([]any, 0, len(intersection))
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
	executeFn := func() any {
		union := make(map[string]struct{})
		for _, key := range keys {
			s, _, wrongType := readSet(cmdCtx.Cache, key)
			if wrongType {
				return resp.ErrWrongType
			}
			for m := range s {
				union[m] = struct{}{}
			}
		}
		result := make([]any, 0, len(union))
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
	executeFn := func() any {
		first, _, wrongType := readSet(cmdCtx.Cache, keys[0])
		if wrongType {
			return resp.ErrWrongType
		}
		diff := make(map[string]struct{}, len(first))
		for m := range first {
			diff[m] = struct{}{}
		}

		for _, key := range keys[1:] {
			s, _, wrongType := readSet(cmdCtx.Cache, key)
			if wrongType {
				return resp.ErrWrongType
			}
			for m := range s {
				delete(diff, m)
			}
		}

		result := make([]any, 0, len(diff))
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

	executeFn := func() any {
		set, _, wrongType := readSet(cmdCtx.Cache, key)
		if wrongType {
			return resp.ErrWrongType
		}
		if set == nil {
			set = make(map[string]struct{})
		}

		added := 0
		for _, member := range members {
			if _, exists := set[member]; !exists {
				set[member] = struct{}{}
				added++
			}
		}

		if err := writeSet(cmdCtx, key, set); err != nil {
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

	executeFn := func() any {
		set, found, wrongType := readSet(cmdCtx.Cache, key)
		if !found {
			return 0
		}
		if wrongType {
			return resp.ErrWrongType
		}

		removed := 0
		for _, member := range members {
			if _, exists := set[member]; exists {
				delete(set, member)
				removed++
			}
		}

		if err := writeSet(cmdCtx, key, set); err != nil {
			return err
		}
		return removed
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSmembers implements SMEMBERS key
func HandleSmembers(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []any{}
		}
		if entry.ValueType != cache.ObjTypeSet {
			return resp.ErrWrongType
		}
		// SMEMBERS doesn't need the map — the encoded form already stores
		// members sorted, so DecodeSetSlice skips the map allocation.
		members, err := cache.DecodeSetSlice(entry.Value)
		if err != nil {
			return resp.ErrWrongType
		}
		result := make([]any, len(members))
		for i, m := range members {
			result[i] = m
		}
		return result
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSismember implements SISMEMBER key member
func HandleSismember(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	member := cmdCtx.Args[1]

	executeFn := func() any {
		set, found, wrongType := readSet(cmdCtx.Cache, key)
		if !found {
			return 0
		}
		if wrongType {
			return resp.ErrWrongType
		}
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

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeSet {
			return resp.ErrWrongType
		}
		n, err := cache.SetLen(entry.Value)
		if err != nil {
			return resp.ErrWrongType
		}
		return n
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSpop implements SPOP key
func HandleSpop(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		set, found, wrongType := readSet(cmdCtx.Cache, key)
		if !found {
			return nil
		}
		if wrongType {
			return resp.ErrWrongType
		}
		if len(set) == 0 {
			return nil
		}

		// Non-deterministic pop matches existing behavior (today's map-range
		// iteration). After Phase 1 the on-disk form is sorted, so we iterate
		// the map we just decoded — still map iteration, still non-deterministic.
		var popped string
		for member := range set {
			popped = member
			break
		}
		delete(set, popped)

		if err := writeSet(cmdCtx, key, set); err != nil {
			return err
		}
		return popped
	}

	return command.Dispatch(cmdCtx, executeFn)
}
