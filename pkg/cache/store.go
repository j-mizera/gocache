package cache

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gammazero/deque"

	apipersistence "gocache/api/persistence"
)

var _ apipersistence.CacheStore = (*Cache)(nil)

// CaptureSnapshot walks all entries and returns a serializable snapshot.
// Caller must hold c.Lock() or ensure exclusive access (e.g. engine.Dispatch).
func (c *Cache) CaptureSnapshot() []apipersistence.SnapshotEntry {
	var entries []apipersistence.SnapshotEntry
	c.Range(func(key string, entry Entry, expiration int64) bool {
		var v any
		switch {
		case entry.Encoding == EncPacked:
			src := c.ResolvePacked(entry)
			buf := make([]byte, len(src))
			copy(buf, src)
			v = buf
		case entry.ValueType == ObjTypeList:
			if dq, ok := entry.Value.(*deque.Deque[string]); ok {
				items := make([]string, 0, dq.Len())
				for i := 0; i < dq.Len(); i++ {
					items = append(items, dq.At(i))
				}
				v = items
			} else {
				v = entry.Value
			}
		case entry.ValueType == ObjTypeSortedSet:
			if z, ok := entry.Value.(*SortedSet); ok {
				v = z.Members()
			} else {
				v = entry.Value
			}
		default:
			v = entry.Value
		}
		entries = append(entries, apipersistence.SnapshotEntry{
			Key:        key,
			ValueType:  apipersistence.ValueType(entry.ValueType),
			Encoding:   apipersistence.Encoding(entry.Encoding),
			Value:      v,
			Expiration: expiration,
		})
		return true
	})
	return entries
}

func (c *Cache) LoadEntry(_ context.Context, e apipersistence.SnapshotEntry) error {
	if e.Encoding == apipersistence.EncodingPacked {
		var buf []byte
		switch v := e.Value.(type) {
		case []byte:
			buf = v
		case string:
			buf = []byte(v)
		default:
			return fmt.Errorf("packed entry has non-byte payload: %T", e.Value)
		}
		c.RawLoadPacked(e.Key, ValueType(e.ValueType), buf, e.Expiration)
		return nil
	}
	value := e.Value
	if e.ValueType == apipersistence.ValueTypeList {
		items, ok := value.([]string)
		if !ok {
			return fmt.Errorf("list entry has non-slice payload: %T", e.Value)
		}
		dq := new(deque.Deque[string])
		dq.Grow(len(items))
		for _, item := range items {
			dq.PushBack(item)
		}
		value = dq
	}
	if e.ValueType == apipersistence.ValueTypeSortedSet {
		members, ok := value.(map[string]float64)
		if !ok {
			return fmt.Errorf("sorted-set entry has non-map payload: %T", e.Value)
		}
		z := NewSortedSet()
		for member, score := range members {
			z.Add(member, score)
		}
		value = z
	}
	c.RawLoad(e.Key, value, e.Expiration)
	return nil
}

// ApplyMutation re-executes a single persisted mutation during AOF
// replay (ADR-0017). Boot is single-threaded so no locking is needed.
func (c *Cache) ApplyMutation(ctx context.Context, m apipersistence.Mutation) error {
	if len(m.Args) == 0 {
		if m.Op == "FLUSHDB" || m.Op == "FLUSHALL" {
			c.Clear(ctx)
			return nil
		}
		return fmt.Errorf("apply %s: missing args", m.Op)
	}

	key := string(m.Args[0])

	switch m.Op {
	case "SET":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply SET: need >= 2 args, got %d", len(m.Args))
		}
		c.RawLoad(key, m.Args[1], parseSetExpiration(m.Args[2:]))

	case "SETNX":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply SETNX: need >= 2 args, got %d", len(m.Args))
		}
		if _, ok := c.RawGet(key); !ok {
			c.RawLoad(key, m.Args[1], 0)
		}

	case "GETSET":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply GETSET: need >= 2 args, got %d", len(m.Args))
		}
		c.RawLoad(key, m.Args[1], 0)

	case "GETDEL":
		c.RawDelete(key)

	case "DEL":
		for _, a := range m.Args {
			c.RawDelete(string(a))
		}

	case "MSET":
		if len(m.Args)%2 != 0 {
			return fmt.Errorf("apply MSET: odd arg count (%d)", len(m.Args))
		}
		for i := 0; i < len(m.Args); i += 2 {
			c.RawLoad(string(m.Args[i]), m.Args[i+1], 0)
		}

	case "APPEND":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply APPEND: need >= 2 args, got %d", len(m.Args))
		}
		existing, ttl := c.resolveBytes(key)
		val := make([]byte, len(existing)+len(m.Args[1]))
		copy(val, existing)
		copy(val[len(existing):], m.Args[1])
		c.RawLoad(key, val, ttl)

	case "INCR":
		return c.applyIncrBy(key, 1)
	case "DECR":
		return c.applyIncrBy(key, -1)
	case "INCRBY":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply INCRBY: need >= 2 args, got %d", len(m.Args))
		}
		delta, err := strconv.ParseInt(string(m.Args[1]), 10, 64)
		if err != nil {
			return fmt.Errorf("apply INCRBY: %w", err)
		}
		return c.applyIncrBy(key, delta)
	case "DECRBY":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply DECRBY: need >= 2 args, got %d", len(m.Args))
		}
		delta, err := strconv.ParseInt(string(m.Args[1]), 10, 64)
		if err != nil {
			return fmt.Errorf("apply DECRBY: %w", err)
		}
		return c.applyIncrBy(key, -delta)
	case "INCRBYFLOAT":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply INCRBYFLOAT: need >= 2 args, got %d", len(m.Args))
		}
		delta, err := strconv.ParseFloat(string(m.Args[1]), 64)
		if err != nil {
			return fmt.Errorf("apply INCRBYFLOAT: %w", err)
		}
		existing, ttl := c.resolveBytes(key)
		var cur float64
		if len(existing) > 0 {
			cur, err = strconv.ParseFloat(strings.TrimSpace(string(existing)), 64)
			if err != nil {
				return fmt.Errorf("apply INCRBYFLOAT: bad existing value: %w", err)
			}
		}
		c.RawLoad(key, []byte(strconv.FormatFloat(cur+delta, 'f', -1, 64)), ttl)

	case "EXPIRE":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply EXPIRE: need >= 2 args, got %d", len(m.Args))
		}
		sec, err := strconv.ParseInt(string(m.Args[1]), 10, 64)
		if err != nil {
			return fmt.Errorf("apply EXPIRE: %w", err)
		}
		c.SetExpiration(key, time.Now().Add(time.Duration(sec)*time.Second).UnixNano())

	case "PEXPIRE":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply PEXPIRE: need >= 2 args, got %d", len(m.Args))
		}
		ms, err := strconv.ParseInt(string(m.Args[1]), 10, 64)
		if err != nil {
			return fmt.Errorf("apply PEXPIRE: %w", err)
		}
		c.SetExpiration(key, time.Now().Add(time.Duration(ms)*time.Millisecond).UnixNano())

	case "RENAME":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply RENAME: need >= 2 args, got %d", len(m.Args))
		}
		c.Rename(key, string(m.Args[1]), c.RawTTL(key))

	case "FLUSHDB", "FLUSHALL":
		c.Clear(ctx)

	// --- hash ---

	case "HSET":
		if len(m.Args) < 3 || (len(m.Args)-1)%2 != 0 {
			return fmt.Errorf("apply HSET: need key + field-value pairs, got %d args", len(m.Args))
		}
		h, ttl := c.getHash(key)
		for i := 1; i+1 < len(m.Args); i += 2 {
			h[string(m.Args[i])] = string(m.Args[i+1])
		}
		c.RawLoad(key, h, ttl)

	case "HDEL":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply HDEL: need >= 2 args, got %d", len(m.Args))
		}
		e, ok := c.RawGet(key)
		if !ok {
			return nil
		}
		var h map[string]string
		if e.Encoding == EncPacked {
			var err error
			h, err = DecodeHash(c.ResolvePacked(e))
			if err != nil {
				return fmt.Errorf("apply HDEL: decode packed hash %q: %w", key, err)
			}
		} else {
			var ok bool
			h, ok = e.Value.(map[string]string)
			if !ok {
				return fmt.Errorf("apply HDEL: key %q is not a hash", key)
			}
		}
		for _, f := range m.Args[1:] {
			delete(h, string(f))
		}
		if len(h) == 0 {
			c.RawDelete(key)
		} else {
			c.RawLoad(key, h, c.RawTTL(key))
		}

	// --- set ---

	case "SADD":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply SADD: need >= 2 args, got %d", len(m.Args))
		}
		s, ttl := c.getSet(key)
		for _, member := range m.Args[1:] {
			s[string(member)] = struct{}{}
		}
		c.RawLoad(key, s, ttl)

	case "SREM":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply SREM: need >= 2 args, got %d", len(m.Args))
		}
		e, ok := c.RawGet(key)
		if !ok {
			return nil
		}
		var s map[string]struct{}
		if e.Encoding == EncPacked {
			var err error
			s, err = DecodeSet(c.ResolvePacked(e))
			if err != nil {
				return fmt.Errorf("apply SREM: decode packed set %q: %w", key, err)
			}
		} else {
			var ok bool
			s, ok = e.Value.(map[string]struct{})
			if !ok {
				return fmt.Errorf("apply SREM: key %q is not a set", key)
			}
		}
		for _, member := range m.Args[1:] {
			delete(s, string(member))
		}
		if len(s) == 0 {
			c.RawDelete(key)
		} else {
			c.RawLoad(key, s, c.RawTTL(key))
		}

	case "SPOP":
		// Non-deterministic: mutation doesn't record which members were
		// popped. We pop arbitrary members to preserve cardinality.
		count := 1
		if len(m.Args) > 1 {
			count, _ = strconv.Atoi(string(m.Args[1]))
		}
		e, ok := c.RawGet(key)
		if !ok {
			return nil
		}
		var s map[string]struct{}
		if e.Encoding == EncPacked {
			var err error
			s, err = DecodeSet(c.ResolvePacked(e))
			if err != nil {
				return fmt.Errorf("apply SPOP: decode packed set %q: %w", key, err)
			}
		} else {
			var ok bool
			s, ok = e.Value.(map[string]struct{})
			if !ok {
				return fmt.Errorf("apply SPOP: key %q is not a set", key)
			}
		}
		i := 0
		for member := range s {
			if i >= count {
				break
			}
			delete(s, member)
			i++
		}
		if len(s) == 0 {
			c.RawDelete(key)
		} else {
			c.RawLoad(key, s, c.RawTTL(key))
		}

	// --- sorted set ---

	case "ZADD":
		if len(m.Args) < 3 {
			return fmt.Errorf("apply ZADD: need >= 3 args, got %d", len(m.Args))
		}
		z, ttl := c.getSortedSet(key)
		i := 1
		for i < len(m.Args) {
			flag := strings.ToUpper(string(m.Args[i]))
			if flag == "NX" || flag == "XX" || flag == "GT" || flag == "LT" || flag == "CH" {
				i++
				continue
			}
			break
		}
		for ; i+1 < len(m.Args); i += 2 {
			score, err := strconv.ParseFloat(string(m.Args[i]), 64)
			if err != nil {
				return fmt.Errorf("apply ZADD: bad score %q: %w", m.Args[i], err)
			}
			z.Add(string(m.Args[i+1]), score)
		}
		c.RawLoad(key, z, ttl)

	case "ZREM":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply ZREM: need >= 2 args, got %d", len(m.Args))
		}
		e, ok := c.RawGet(key)
		if !ok {
			return nil
		}
		var z *SortedSet
		if e.Encoding == EncPacked {
			var err error
			z, err = DecodeZSet(c.ResolvePacked(e))
			if err != nil {
				return fmt.Errorf("apply ZREM: decode packed zset %q: %w", key, err)
			}
		} else {
			var ok bool
			z, ok = e.Value.(*SortedSet)
			if !ok {
				return fmt.Errorf("apply ZREM: key %q is not a sorted set", key)
			}
		}
		for _, member := range m.Args[1:] {
			z.Remove(string(member))
		}
		if z.Card() == 0 {
			c.RawDelete(key)
		} else {
			c.RawLoad(key, z, c.RawTTL(key))
		}

	// --- list ---

	case "LPUSH":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply LPUSH: need >= 2 args, got %d", len(m.Args))
		}
		dq, ttl := c.getList(key)
		for _, v := range m.Args[1:] {
			dq.PushFront(string(v))
		}
		c.RawLoad(key, dq, ttl)

	case "RPUSH":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply RPUSH: need >= 2 args, got %d", len(m.Args))
		}
		dq, ttl := c.getList(key)
		for _, v := range m.Args[1:] {
			dq.PushBack(string(v))
		}
		c.RawLoad(key, dq, ttl)

	case "LPOP":
		return c.applyPop(key, m.Args, true)
	case "RPOP":
		return c.applyPop(key, m.Args, false)

	default:
		return fmt.Errorf("unknown mutation op: %s", m.Op)
	}
	return nil
}

// --- ApplyMutation helpers ---

func (c *Cache) resolveBytes(key string) ([]byte, int64) {
	e, ok := c.RawGet(key)
	if !ok {
		return nil, 0
	}
	ttl := c.RawTTL(key)
	if e.Encoding == EncPacked {
		src := c.ResolvePacked(e)
		buf := make([]byte, len(src))
		copy(buf, src)
		return buf, ttl
	}
	switch v := e.Value.(type) {
	case []byte:
		buf := make([]byte, len(v))
		copy(buf, v)
		return buf, ttl
	case string:
		return []byte(v), ttl
	}
	return nil, ttl
}

func (c *Cache) applyIncrBy(key string, delta int64) error {
	existing, ttl := c.resolveBytes(key)
	var cur int64
	if len(existing) > 0 {
		var err error
		cur, err = strconv.ParseInt(strings.TrimSpace(string(existing)), 10, 64)
		if err != nil {
			return fmt.Errorf("apply INCR/DECRBY %q: %w", key, err)
		}
	}
	c.RawLoad(key, []byte(strconv.FormatInt(cur+delta, 10)), ttl)
	return nil
}

func (c *Cache) applyPop(key string, args [][]byte, left bool) error {
	count := 1
	if len(args) > 1 {
		count, _ = strconv.Atoi(string(args[1]))
	}
	e, ok := c.RawGet(key)
	if !ok {
		return nil
	}
	dq, ok := e.Value.(*deque.Deque[string])
	if !ok {
		return fmt.Errorf("apply L/RPOP: key %q is not a list", key)
	}
	for i := 0; i < count && dq.Len() > 0; i++ {
		if left {
			dq.PopFront()
		} else {
			dq.PopBack()
		}
	}
	if dq.Len() == 0 {
		c.RawDelete(key)
	} else {
		c.RawLoad(key, dq, c.RawTTL(key))
	}
	return nil
}

func (c *Cache) getHash(key string) (map[string]string, int64) {
	e, ok := c.RawGet(key)
	if !ok {
		return make(map[string]string), 0
	}
	if e.Encoding == EncPacked {
		h, err := DecodeHash(c.ResolvePacked(e))
		if err != nil {
			return make(map[string]string), c.RawTTL(key)
		}
		return h, c.RawTTL(key)
	}
	if h, ok := e.Value.(map[string]string); ok {
		return h, c.RawTTL(key)
	}
	return make(map[string]string), c.RawTTL(key)
}

func (c *Cache) getSet(key string) (map[string]struct{}, int64) {
	e, ok := c.RawGet(key)
	if !ok {
		return make(map[string]struct{}), 0
	}
	if e.Encoding == EncPacked {
		s, err := DecodeSet(c.ResolvePacked(e))
		if err != nil {
			return make(map[string]struct{}), c.RawTTL(key)
		}
		return s, c.RawTTL(key)
	}
	if s, ok := e.Value.(map[string]struct{}); ok {
		return s, c.RawTTL(key)
	}
	return make(map[string]struct{}), c.RawTTL(key)
}

func (c *Cache) getSortedSet(key string) (*SortedSet, int64) {
	e, ok := c.RawGet(key)
	if !ok {
		return NewSortedSet(), 0
	}
	if e.Encoding == EncPacked {
		z, err := DecodeZSet(c.ResolvePacked(e))
		if err != nil {
			return NewSortedSet(), c.RawTTL(key)
		}
		return z, c.RawTTL(key)
	}
	if z, ok := e.Value.(*SortedSet); ok {
		return z, c.RawTTL(key)
	}
	return NewSortedSet(), c.RawTTL(key)
}

func (c *Cache) getList(key string) (*deque.Deque[string], int64) {
	e, ok := c.RawGet(key)
	if !ok {
		return new(deque.Deque[string]), 0
	}
	if dq, ok := e.Value.(*deque.Deque[string]); ok {
		return dq, c.RawTTL(key)
	}
	return new(deque.Deque[string]), c.RawTTL(key)
}

func parseSetExpiration(opts [][]byte) int64 {
	for i := 0; i+1 < len(opts); i++ {
		switch strings.ToUpper(string(opts[i])) {
		case "EX":
			sec, _ := strconv.ParseInt(string(opts[i+1]), 10, 64)
			if sec > 0 {
				return time.Now().Add(time.Duration(sec) * time.Second).UnixNano()
			}
		case "PX":
			ms, _ := strconv.ParseInt(string(opts[i+1]), 10, 64)
			if ms > 0 {
				return time.Now().Add(time.Duration(ms) * time.Millisecond).UnixNano()
			}
		case "EXAT":
			sec, _ := strconv.ParseInt(string(opts[i+1]), 10, 64)
			if sec > 0 {
				return sec * int64(time.Second)
			}
		case "PXAT":
			ms, _ := strconv.ParseInt(string(opts[i+1]), 10, 64)
			if ms > 0 {
				return ms * int64(time.Millisecond)
			}
		}
	}
	return 0
}
