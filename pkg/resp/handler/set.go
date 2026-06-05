package handler

import (
	apicommand "gocache/api/command"
	"gocache/pkg/cache"
	"gocache/pkg/cache/packed"
	"gocache/pkg/command"
)

// setMapSize walks a map[string]struct{} once for its estimateSize-equivalent
// payload size. Used at packed→native promotion; subsequent mutations
// track size incrementally and never re-walk.
func setMapSize(set map[string]struct{}) int64 {
	var size int64
	for m := range set {
		size += int64(len(m)) + cache.SetMemberOverhead
	}
	return size
}

// Set commands operate on two encodings:
//
//   EncPacked: cmdCtx.Cache.ResolvePacked(entry) — a sorted, length-prefixed buffer.
//             Mutations go through packed.Set*. SADD keeps the buffer
//             sorted via binary insertion.
//   EncNative: entry.Value.(map[string]struct{}) — the Go map shape used
//             for large or promoted sets.
//
// Single-set mutations (SADD, SREM) and reads (SISMEMBER, SCARD, SPOP,
// SMEMBERS) dispatch on Encoding. Multi-set ops (SINTER, SUNION, SDIFF)
// materialise inputs to map[string]struct{} — the intermediate maps they
// build are unavoidable, and materialising once per input set is cheaper
// than repeatedly scanning both packed and native shapes.

// getSetAsMap returns the set at key as a map[string]struct{}. Packed
// entries are materialised; the returned map is a fresh copy so callers
// can mutate it freely. Returns (nil, nil) for a missing key.
func getSetAsMap(c *cache.Cache, key string) (map[string]struct{}, error) {
	entry, found := c.RawGet(key)
	if !found {
		return nil, nil
	}
	if entry.ValueType != cache.ObjTypeSet {
		return nil, apicommand.ErrWrongType
	}
	switch entry.Encoding {
	case cache.EncPacked:
		m, err := packed.SetToMap(c.ResolvePacked(entry))
		if err != nil {
			return nil, err
		}
		return m, nil
	default:
		// Callers of multi-set ops mutate the returned map; return a copy.
		src := entry.Value.(map[string]struct{})
		out := make(map[string]struct{}, len(src))
		for k := range src {
			out[k] = struct{}{}
		}
		return out, nil
	}
}

// HandleSinter implements SINTER key [key ...]
func HandleSinter(cmdCtx *command.Context) apicommand.Result {
	keys := cmdCtx.Args
	cmdCtx.TouchedShards = cmdCtx.Cache.TouchedShards(keys)
	executeFn := func() any {
		intersection, err := getSetAsMap(cmdCtx.Cache, keys[0])
		if err != nil {
			return err
		}
		for _, key := range keys[1:] {
			s, err := getSetAsMap(cmdCtx.Cache, key)
			if err != nil {
				return err
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
func HandleSunion(cmdCtx *command.Context) apicommand.Result {
	keys := cmdCtx.Args
	cmdCtx.TouchedShards = cmdCtx.Cache.TouchedShards(keys)
	executeFn := func() any {
		union := make(map[string]struct{})
		for _, key := range keys {
			s, err := getSetAsMap(cmdCtx.Cache, key)
			if err != nil {
				return err
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
func HandleSdiff(cmdCtx *command.Context) apicommand.Result {
	keys := cmdCtx.Args
	cmdCtx.TouchedShards = cmdCtx.Cache.TouchedShards(keys)
	executeFn := func() any {
		diff, err := getSetAsMap(cmdCtx.Cache, keys[0])
		if err != nil {
			return err
		}
		for _, key := range keys[1:] {
			s, err := getSetAsMap(cmdCtx.Cache, key)
			if err != nil {
				return err
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
func HandleSadd(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	members := cmdCtx.Args[1:]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return saddStartPacked(cmdCtx, key, members)
		}
		if entry.ValueType != cache.ObjTypeSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			return saddPacked(cmdCtx, key, cmdCtx.Cache.ResolvePacked(entry), members)
		default:
			return saddNative(cmdCtx, key, entry.Value.(map[string]struct{}), members)
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

func saddStartPacked(cmdCtx *command.Context, key string, members []string) any {
	return saddPacked(cmdCtx, key, packed.SetNew(), members)
}

func saddPacked(cmdCtx *command.Context, key string, buf []byte, members []string) any {
	t := cmdCtx.Cache.PackedThresholds()
	ttl := cmdCtx.Cache.RawTTL(key)
	newBuf, added, promoted, err := packed.SetAddBatch(buf, members, t.SetMaxEntries, t.SetMaxValue)
	if err != nil {
		return err
	}
	if promoted {
		set, perr := packed.SetToMap(newBuf)
		if perr != nil {
			return perr
		}
		if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, set, setMapSize(set), ttl); err != nil {
			return err
		}
		return added
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Telemetry(), key, cache.ObjTypeSet, newBuf, ttl); err != nil {
		return err
	}
	return added
}

func saddNative(cmdCtx *command.Context, key string, set map[string]struct{}, members []string) any {
	added := 0
	ttl := cmdCtx.Cache.RawTTL(key)
	size := cmdCtx.Cache.NativeSize(key)
	for _, m := range members {
		if _, exists := set[m]; !exists {
			set[m] = struct{}{}
			added++
			size += int64(len(m)) + cache.SetMemberOverhead
		}
	}
	if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, set, size, ttl); err != nil {
		return err
	}
	return added
}

// HandleSrem implements SREM key member [member ...]
func HandleSrem(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	members := cmdCtx.Args[1:]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			buf := cmdCtx.Cache.ResolvePacked(entry)
			removed := 0
			for _, m := range members {
				var rm bool
				var err error
				buf, rm, err = packed.SetRemove(buf, m)
				if err != nil {
					return err
				}
				if rm {
					removed++
				}
			}
			n, _ := packed.SetLen(buf)
			if n == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				ttl := cmdCtx.Cache.RawTTL(key)
				if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Telemetry(), key, cache.ObjTypeSet, buf, ttl); err != nil {
					return err
				}
			}
			return removed
		default:
			set := entry.Value.(map[string]struct{})
			size := cmdCtx.Cache.NativeSize(key)
			removed := 0
			for _, m := range members {
				if _, exists := set[m]; exists {
					delete(set, m)
					removed++
					size -= int64(len(m)) + cache.SetMemberOverhead
				}
			}
			if len(set) == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				ttl := cmdCtx.Cache.RawTTL(key)
				if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, set, size, ttl); err != nil {
					return err
				}
			}
			return removed
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSmembers implements SMEMBERS key
func HandleSmembers(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []any{}
		}
		if entry.ValueType != cache.ObjTypeSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			members, err := packed.SetMembers(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			result := make([]any, 0, len(members))
			for _, m := range members {
				result = append(result, m)
			}
			return result
		default:
			set := entry.Value.(map[string]struct{})
			result := make([]any, 0, len(set))
			for member := range set {
				result = append(result, member)
			}
			return result
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSismember implements SISMEMBER key member
func HandleSismember(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	member := cmdCtx.Args[1]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			ok, err := packed.SetContains(cmdCtx.Cache.ResolvePacked(entry), member)
			if err != nil {
				return err
			}
			if ok {
				return 1
			}
			return 0
		default:
			set := entry.Value.(map[string]struct{})
			if _, exists := set[member]; exists {
				return 1
			}
			return 0
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleScard implements SCARD key
func HandleScard(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			n, err := packed.SetLen(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			return n
		default:
			return len(entry.Value.(map[string]struct{}))
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleSpop implements SPOP key
func HandleSpop(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			// Pop the first (lex-smallest) member — SPOP is documented as
			// "a random element"; native map-range is pseudo-random, but
			// lex-first is equally valid under the spec and avoids an
			// intermediate []string.
			buf := cmdCtx.Cache.ResolvePacked(entry)
			var popped string
			err := packed.SetIterate(buf, func(m []byte) bool {
				popped = string(m)
				return false
			})
			if err != nil {
				return err
			}
			if popped == "" {
				n, _ := packed.SetLen(buf)
				if n == 0 {
					return nil
				}
				// An empty string member IS the "first" — fall through.
			}
			buf, _, err = packed.SetRemove(buf, popped)
			if err != nil {
				return err
			}
			n, _ := packed.SetLen(buf)
			if n == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				ttl := cmdCtx.Cache.RawTTL(key)
				if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Telemetry(), key, cache.ObjTypeSet, buf, ttl); err != nil {
					return err
				}
			}
			return popped
		default:
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
				ttl := cmdCtx.Cache.RawTTL(key)
				newSize := cmdCtx.Cache.NativeSize(key) - int64(len(popped)) - cache.SetMemberOverhead
				if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Telemetry(), key, set, newSize, ttl); err != nil {
					return err
				}
			}
			return popped
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}
