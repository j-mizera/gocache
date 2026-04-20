package handler

import (
	"strconv"
	"strings"

	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// readZSet decodes the byte-encoded sorted set at key. Returns the reusable
// SortedSet for mutating handlers (ZADD, ZREM) and the score-sorted pair
// slice for read-only handlers (ZRANGE, ZRANK, ZCOUNT). Split via two
// helpers so ZRANGE doesn't pay the map-allocation of a full SortedSet
// restore when it only needs a linear walk.
func readZSet(c *cache.Cache, key string) (*cache.SortedSet, bool, bool) {
	entry, ok := c.RawGet(key)
	if !ok {
		return nil, false, false
	}
	if entry.ValueType != cache.ObjTypeSortedSet {
		return nil, true, true
	}
	b, _ := entry.Value.([]byte)
	z, err := cache.DecodeZSet(b)
	if err != nil {
		return nil, true, true
	}
	return z, true, false
}

// readZSetPairs returns the pre-sorted pair slice directly — cheaper than
// restoring a SortedSet for read-only handlers.
func readZSetPairs(c *cache.Cache, key string) ([]cache.ScoredMember, bool, bool) {
	entry, ok := c.RawGet(key)
	if !ok {
		return nil, false, false
	}
	if entry.ValueType != cache.ObjTypeSortedSet {
		return nil, true, true
	}
	b, _ := entry.Value.([]byte)
	pairs, err := cache.DecodeZSetPairs(b)
	if err != nil {
		return nil, true, true
	}
	return pairs, true, false
}

// writeZSet encodes z and stores it under key. An empty zset deletes the key.
func writeZSet(cmdCtx *command.Context, key string, z *cache.SortedSet) error {
	if z.Card() == 0 {
		cmdCtx.Cache.RawDelete(key)
		return nil
	}
	encoded, err := cache.EncodeZSet(z)
	if err != nil {
		return err
	}
	return cmdCtx.Cache.RawSetTyped(cmdCtx.Context(), key, cache.ObjTypeSortedSet, encoded, 0)
}

// HandleZadd implements ZADD key score member [score member ...]
func HandleZadd(cmdCtx *command.Context) command.Result {
	if (len(cmdCtx.Args)-1)%2 != 0 {
		return command.Result{Value: resp.ErrArgs("zadd")}
	}

	key := cmdCtx.Args[0]

	executeFn := func() any {
		zset, _, wrongType := readZSet(cmdCtx.Cache, key)
		if wrongType {
			return resp.ErrWrongType
		}
		if zset == nil {
			zset = cache.NewSortedSet()
		}

		added := 0
		for i := 1; i < len(cmdCtx.Args); i += 2 {
			scoreStr := cmdCtx.Args[i]
			member := cmdCtx.Args[i+1]
			score, err := strconv.ParseFloat(scoreStr, 64)
			if err != nil {
				return resp.ErrNotFloat
			}
			if zset.Add(member, score) {
				added++
			}
		}

		if err := writeZSet(cmdCtx, key, zset); err != nil {
			return err
		}
		return added
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZrem implements ZREM key member [member ...]
func HandleZrem(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	members := cmdCtx.Args[1:]

	executeFn := func() any {
		zset, found, wrongType := readZSet(cmdCtx.Cache, key)
		if !found {
			return 0
		}
		if wrongType {
			return resp.ErrWrongType
		}

		removed := 0
		for _, member := range members {
			if zset.Remove(member) {
				removed++
			}
		}

		if err := writeZSet(cmdCtx, key, zset); err != nil {
			return err
		}
		return removed
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZscore implements ZSCORE key member
func HandleZscore(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	member := cmdCtx.Args[1]

	executeFn := func() any {
		pairs, found, wrongType := readZSetPairs(cmdCtx.Cache, key)
		if !found {
			return nil
		}
		if wrongType {
			return resp.ErrWrongType
		}
		for _, p := range pairs {
			if p.Member == member {
				return strconv.FormatFloat(p.Score, 'f', -1, 64)
			}
		}
		return nil
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZcard implements ZCARD key
func HandleZcard(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() any {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}
		if entry.ValueType != cache.ObjTypeSortedSet {
			return resp.ErrWrongType
		}
		n, err := cache.ZSetLen(entry.Value.([]byte))
		if err != nil {
			return resp.ErrWrongType
		}
		return n
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZrange implements ZRANGE key start stop [WITHSCORES]
func HandleZrange(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	start, err1 := strconv.Atoi(cmdCtx.Args[1])
	stop, err2 := strconv.Atoi(cmdCtx.Args[2])

	if err1 != nil || err2 != nil {
		return command.Result{Err: resp.ErrNotInteger}
	}

	withScores := false
	if len(cmdCtx.Args) > 3 {
		if strings.ToUpper(cmdCtx.Args[3]) != "WITHSCORES" {
			return command.Result{Value: resp.ErrSyntax()}
		}
		withScores = true
	}

	executeFn := func() any {
		pairs, found, wrongType := readZSetPairs(cmdCtx.Cache, key)
		if !found {
			return []any{}
		}
		if wrongType {
			return resp.ErrWrongType
		}
		length := len(pairs)

		// Redis index semantics: negative = from end, clamped to bounds.
		if start < 0 {
			start = length + start
		}
		if stop < 0 {
			stop = length + stop
		}
		if start < 0 {
			start = 0
		}
		if stop >= length {
			stop = length - 1
		}
		if start > stop || start >= length {
			return []any{}
		}
		slice := pairs[start : stop+1]

		if withScores {
			result := make([]any, 0, len(slice)*2)
			for _, sm := range slice {
				result = append(result, sm.Member, sm.Score)
			}
			return result
		}
		result := make([]any, 0, len(slice))
		for _, sm := range slice {
			result = append(result, sm.Member)
		}
		return result
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZrank implements ZRANK key member
func HandleZrank(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	member := cmdCtx.Args[1]

	executeFn := func() any {
		pairs, found, wrongType := readZSetPairs(cmdCtx.Cache, key)
		if !found {
			return nil
		}
		if wrongType {
			return resp.ErrWrongType
		}
		for i, p := range pairs {
			if p.Member == member {
				return i
			}
		}
		return nil
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZcount implements ZCOUNT key min max
func HandleZcount(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	min, err1 := strconv.ParseFloat(cmdCtx.Args[1], 64)
	max, err2 := strconv.ParseFloat(cmdCtx.Args[2], 64)

	if err1 != nil || err2 != nil {
		return command.Result{Err: resp.ErrNotFloat}
	}

	executeFn := func() any {
		pairs, found, wrongType := readZSetPairs(cmdCtx.Cache, key)
		if !found {
			return 0
		}
		if wrongType {
			return resp.ErrWrongType
		}
		count := 0
		for _, p := range pairs {
			if p.Score >= min && p.Score <= max {
				count++
			}
		}
		return count
	}

	return command.Dispatch(cmdCtx, executeFn)
}
