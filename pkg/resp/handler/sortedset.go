package handler

import (
	"strconv"
	"strings"

	apicommand "gocache/api/command"
	"gocache/pkg/cache"
	"gocache/pkg/cache/packed"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)


// Sorted-set commands operate on two encodings:
//
//   EncPacked: cmdCtx.Cache.ResolvePacked(entry) — a packed.ZSet buffer sorted by
//             (score asc, member asc). ZADD inserts at the correct sort
//             position; ZRANGE/ZRANGEBYSCORE walk forward.
//   EncNative: entry.Value.(*cache.SortedSet) — the skiplist-style shape
//             used by large zsets.

// HandleZadd implements ZADD key score member [score member ...]
func HandleZadd(cmdCtx *command.Context) apicommand.Result {
	if (len(cmdCtx.Args)-1)%2 != 0 {
		return apicommand.Result{Value: resp.ErrArgs("zadd")}
	}

	key := cmdCtx.Args[0]

	// Pre-parse scores so bad input fails cleanly before touching the cache.
	pairs := make([]cache.ScoredMember, 0, (len(cmdCtx.Args)-1)/2)
	for i := 1; i < len(cmdCtx.Args); i += 2 {
		score, err := strconv.ParseFloat(cmdCtx.Args[i], 64)
		if err != nil {
			return apicommand.Result{Err: apicommand.ErrNotFloat}
		}
		pairs = append(pairs, cache.ScoredMember{Member: cmdCtx.Args[i+1], Score: score})
	}

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return zaddStartPacked(cmdCtx, key, pairs)
		}
		if entry.ValueType != cache.ObjTypeSortedSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			return zaddPacked(cmdCtx, key, cmdCtx.Cache.ResolvePacked(entry), pairs)
		default:
			return zaddNative(cmdCtx, key, entry.Value.(*cache.SortedSet), pairs)
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

func zaddStartPacked(cmdCtx *command.Context, key string, pairs []cache.ScoredMember) any {
	return zaddPacked(cmdCtx, key, packed.ZSetNew(), pairs)
}

func zaddPacked(cmdCtx *command.Context, key string, buf []byte, pairs []cache.ScoredMember) any {
	t := cmdCtx.Cache.PackedThresholds()
	ttl := cmdCtx.Cache.RawTTL(key)
	added := 0
	for i, p := range pairs {
		var addedOne, promoted bool
		var err error
		buf, addedOne, _, promoted, err = packed.ZSetAdd(buf, p.Member, p.Score, t.ZSetMaxEntries, t.ZSetMaxValue)
		if err != nil {
			return err
		}
		if addedOne {
			added++
		}
		if promoted {
			z, perr := packed.ZSetToNative(buf)
			if perr != nil {
				return perr
			}
			size := z.EstimateSize()
			for _, rest := range pairs[i+1:] {
				if z.Add(rest.Member, rest.Score) {
					added++
					size += int64(len(rest.Member)) + cache.ZSetMemberOverhead
				}
			}
			if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, z, size, ttl); err != nil {
				return err
			}
			return added
		}
	}
	if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeSortedSet, buf, ttl); err != nil {
		return err
	}
	return added
}

func zaddNative(cmdCtx *command.Context, key string, z *cache.SortedSet, pairs []cache.ScoredMember) any {
	added := 0
	ttl := cmdCtx.Cache.RawTTL(key)
	size := cmdCtx.Cache.NativeSize(key)
	for _, p := range pairs {
		if z.Add(p.Member, p.Score) {
			added++
			size += int64(len(p.Member)) + cache.ZSetMemberOverhead
		}
	}
	if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, z, size, ttl); err != nil {
		return err
	}
	return added
}

// HandleZrem implements ZREM key member [member ...]
func HandleZrem(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	members := cmdCtx.Args[1:]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeSortedSet {
			return apicommand.ErrWrongType
		}

		switch entry.Encoding {
		case cache.EncPacked:
			buf := cmdCtx.Cache.ResolvePacked(entry)
			removed := 0
			for _, m := range members {
				var rm bool
				var err error
				buf, rm, err = packed.ZSetRemove(buf, m)
				if err != nil {
					return err
				}
				if rm {
					removed++
				}
			}
			n, _ := packed.ZSetLen(buf)
			if n == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				ttl := cmdCtx.Cache.RawTTL(key)
				if err := cmdCtx.Cache.RawSetPacked(cmdCtx.Context(), key, cache.ObjTypeSortedSet, buf, ttl); err != nil {
					return err
				}
			}
			return removed

		default:
			zset := entry.Value.(*cache.SortedSet)
			size := cmdCtx.Cache.NativeSize(key)
			removed := 0
			for _, m := range members {
				if zset.Remove(m) {
					removed++
					size -= int64(len(m)) + cache.ZSetMemberOverhead
				}
			}
			if zset.Card() == 0 {
				cmdCtx.Cache.RawDelete(key)
			} else {
				ttl := cmdCtx.Cache.RawTTL(key)
				if err := cmdCtx.Cache.RawSetNativeWithSize(cmdCtx.Context(), key, zset, size, ttl); err != nil {
					return err
				}
			}
			return removed
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZscore implements ZSCORE key member
func HandleZscore(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	member := cmdCtx.Args[1]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeSortedSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			score, found, err := packed.ZSetScoreOf(cmdCtx.Cache.ResolvePacked(entry), member)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			return strconv.FormatFloat(score, 'f', -1, 64)
		default:
			zset := entry.Value.(*cache.SortedSet)
			if score, exists := zset.Score(member); exists {
				return strconv.FormatFloat(score, 'f', -1, 64)
			}
			return nil
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZcard implements ZCARD key
func HandleZcard(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeSortedSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			n, err := packed.ZSetLen(cmdCtx.Cache.ResolvePacked(entry))
			if err != nil {
				return err
			}
			return n
		default:
			return entry.Value.(*cache.SortedSet).Card()
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZrange implements ZRANGE key start stop [WITHSCORES]
func HandleZrange(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	start, err1 := strconv.Atoi(cmdCtx.Args[1])
	stop, err2 := strconv.Atoi(cmdCtx.Args[2])
	if err1 != nil || err2 != nil {
		return apicommand.Result{Err: apicommand.ErrNotInteger}
	}

	withScores := false
	if len(cmdCtx.Args) > 3 {
		if strings.ToUpper(cmdCtx.Args[3]) != "WITHSCORES" {
			return apicommand.Result{Value: resp.ErrSyntax()}
		}
		withScores = true
	}

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []any{}
		}
		if entry.ValueType != cache.ObjTypeSortedSet {
			return apicommand.ErrWrongType
		}

		var members []cache.ScoredMember
		switch entry.Encoding {
		case cache.EncPacked:
			var err error
			members, err = packed.ZSetRangeByIndex(cmdCtx.Cache.ResolvePacked(entry), start, stop)
			if err != nil {
				return err
			}
		default:
			members = entry.Value.(*cache.SortedSet).Range(start, stop)
		}

		if withScores {
			result := make([]any, 0, len(members)*2)
			for _, sm := range members {
				result = append(result, sm.Member, sm.Score)
			}
			return result
		}
		result := make([]any, 0, len(members))
		for _, sm := range members {
			result = append(result, sm.Member)
		}
		return result
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZrank implements ZRANK key member
func HandleZrank(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	member := cmdCtx.Args[1]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}
		if entry.ValueType != cache.ObjTypeSortedSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			rank, _, found, err := packed.ZSetRank(cmdCtx.Cache.ResolvePacked(entry), member)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			return rank
		default:
			zset := entry.Value.(*cache.SortedSet)
			if rank, exists := zset.Rank(member); exists {
				return rank
			}
			return nil
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZcount implements ZCOUNT key min max
func HandleZcount(cmdCtx *command.Context) apicommand.Result {
	key := cmdCtx.Args[0]
	min, err1 := strconv.ParseFloat(cmdCtx.Args[1], 64)
	max, err2 := strconv.ParseFloat(cmdCtx.Args[2], 64)
	if err1 != nil || err2 != nil {
		return apicommand.Result{Err: apicommand.ErrNotFloat}
	}

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeSortedSet {
			return apicommand.ErrWrongType
		}
		switch entry.Encoding {
		case cache.EncPacked:
			n, err := packed.ZSetCountByScore(cmdCtx.Cache.ResolvePacked(entry), min, max)
			if err != nil {
				return err
			}
			return n
		default:
			return entry.Value.(*cache.SortedSet).Count(min, max)
		}
	}

	return command.Dispatch(cmdCtx, executeFn)
}
