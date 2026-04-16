package handler

import (
	"strconv"
	"strings"

	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// HandleZadd implements ZADD key score member [score member ...]
func HandleZadd(cmdCtx *command.Context) command.Result {
	if (len(cmdCtx.Args)-1)%2 != 0 {
		return command.Result{Value: resp.ErrArgs("zadd")}
	}

	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		var zset *cache.SortedSet
		added := 0

		if !found {
			zset = cache.NewSortedSet()
		} else {
			if entry.ValueType != cache.ObjTypeSortedSet {
				return resp.ErrWrongType
			}
			zset = entry.Value.(*cache.SortedSet)
		}

		// Process score-member pairs
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

		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, zset, 0); err != nil {
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

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}

		if entry.ValueType != cache.ObjTypeSortedSet {
			return resp.ErrWrongType
		}

		zset := entry.Value.(*cache.SortedSet)
		removed := 0

		for _, member := range members {
			if zset.Remove(member) {
				removed++
			}
		}

		if zset.Card() == 0 {
			cmdCtx.Cache.RawDelete(key)
		} else {
			if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), key, zset, 0); err != nil {
				return err
			}
		}

		return removed
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZscore implements ZSCORE key member
func HandleZscore(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	member := cmdCtx.Args[1]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}

		if entry.ValueType != cache.ObjTypeSortedSet {
			return resp.ErrWrongType
		}

		zset := entry.Value.(*cache.SortedSet)
		if score, exists := zset.Score(member); exists {
			return strconv.FormatFloat(score, 'f', -1, 64)
		}
		return nil
	}

	return command.Dispatch(cmdCtx, executeFn)
}

// HandleZcard implements ZCARD key
func HandleZcard(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}

		if entry.ValueType != cache.ObjTypeSortedSet {
			return resp.ErrWrongType
		}

		zset := entry.Value.(*cache.SortedSet)
		return zset.Card()
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

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return []interface{}{}
		}

		if entry.ValueType != cache.ObjTypeSortedSet {
			return resp.ErrWrongType
		}

		zset := entry.Value.(*cache.SortedSet)
		members := zset.Range(start, stop)

		if withScores {
			result := make([]interface{}, 0, len(members)*2)
			for _, sm := range members {
				result = append(result, sm.Member, sm.Score)
			}
			return result
		}

		result := make([]interface{}, 0, len(members))
		for _, sm := range members {
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

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return nil
		}

		if entry.ValueType != cache.ObjTypeSortedSet {
			return resp.ErrWrongType
		}

		zset := entry.Value.(*cache.SortedSet)
		if rank, exists := zset.Rank(member); exists {
			return rank
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

	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return 0
		}

		if entry.ValueType != cache.ObjTypeSortedSet {
			return resp.ErrWrongType
		}

		zset := entry.Value.(*cache.SortedSet)
		return zset.Count(min, max)
	}

	return command.Dispatch(cmdCtx, executeFn)
}
