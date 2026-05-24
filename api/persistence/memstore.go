package persistence

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MemoryStore is a reference CacheStore for plugin tests. It uses only
// API types and stdlib — no pkg/cache dependency. Values are stored in
// their SnapshotEntry-native representations: string/[]byte for bytes,
// []string for lists, map[string]string for hashes, map[string]struct{}
// for sets, map[string]float64 for sorted sets.
type MemoryStore struct {
	items map[string]*memEntry
}

type memEntry struct {
	value     any
	valueType ValueType
	expireAt  int64
}

// NewMemoryStore returns a ready-to-use test store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string]*memEntry)}
}

// Get returns the raw value and true, or nil and false.
func (s *MemoryStore) Get(key string) (any, bool) {
	e, ok := s.items[key]
	if !ok {
		return nil, false
	}
	return e.value, true
}

// GetString returns the string value of a bytes key.
func (s *MemoryStore) GetString(key string) (string, bool) {
	e, ok := s.items[key]
	if !ok {
		return "", false
	}
	switch v := e.value.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	return "", false
}

// Len returns the number of stored keys.
func (s *MemoryStore) Len() int { return len(s.items) }

// --- CacheStore interface ---

func (s *MemoryStore) CaptureSnapshot() []SnapshotEntry {
	entries := make([]SnapshotEntry, 0, len(s.items))
	for key, e := range s.items {
		entries = append(entries, SnapshotEntry{
			Key:        key,
			ValueType:  e.valueType,
			Encoding:   EncodingNative,
			Value:      e.value,
			Expiration: e.expireAt,
		})
	}
	return entries
}

func (s *MemoryStore) LoadEntry(_ context.Context, e SnapshotEntry) error {
	s.items[e.Key] = &memEntry{
		value:     e.Value,
		valueType: e.ValueType,
		expireAt:  e.Expiration,
	}
	return nil
}

func (s *MemoryStore) Clear(_ context.Context) {
	s.items = make(map[string]*memEntry)
}

func (s *MemoryStore) ApplyMutation(_ context.Context, m Mutation) error {
	if len(m.Args) == 0 {
		if m.Op == "FLUSHDB" || m.Op == "FLUSHALL" {
			s.items = make(map[string]*memEntry)
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
		s.items[key] = &memEntry{
			value:     m.Args[1],
			valueType: ValueTypeBytes,
			expireAt:  parseExpiration(m.Args[2:]),
		}

	case "SETNX":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply SETNX: need >= 2 args, got %d", len(m.Args))
		}
		if _, ok := s.items[key]; !ok {
			s.items[key] = &memEntry{value: m.Args[1], valueType: ValueTypeBytes}
		}

	case "GETSET":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply GETSET: need >= 2 args, got %d", len(m.Args))
		}
		s.items[key] = &memEntry{value: m.Args[1], valueType: ValueTypeBytes}

	case "GETDEL":
		delete(s.items, key)

	case "DEL":
		for _, a := range m.Args {
			delete(s.items, string(a))
		}

	case "MSET":
		if len(m.Args)%2 != 0 {
			return fmt.Errorf("apply MSET: odd arg count (%d)", len(m.Args))
		}
		for i := 0; i < len(m.Args); i += 2 {
			s.items[string(m.Args[i])] = &memEntry{value: m.Args[i+1], valueType: ValueTypeBytes}
		}

	case "APPEND":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply APPEND: need >= 2 args, got %d", len(m.Args))
		}
		existing, ttl := s.resolveBytes(key)
		val := make([]byte, len(existing)+len(m.Args[1]))
		copy(val, existing)
		copy(val[len(existing):], m.Args[1])
		s.items[key] = &memEntry{value: val, valueType: ValueTypeBytes, expireAt: ttl}

	case "INCR":
		return s.applyIncrBy(key, 1)
	case "DECR":
		return s.applyIncrBy(key, -1)
	case "INCRBY":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply INCRBY: need >= 2 args, got %d", len(m.Args))
		}
		delta, err := strconv.ParseInt(string(m.Args[1]), 10, 64)
		if err != nil {
			return fmt.Errorf("apply INCRBY: %w", err)
		}
		return s.applyIncrBy(key, delta)
	case "DECRBY":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply DECRBY: need >= 2 args, got %d", len(m.Args))
		}
		delta, err := strconv.ParseInt(string(m.Args[1]), 10, 64)
		if err != nil {
			return fmt.Errorf("apply DECRBY: %w", err)
		}
		return s.applyIncrBy(key, -delta)
	case "INCRBYFLOAT":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply INCRBYFLOAT: need >= 2 args, got %d", len(m.Args))
		}
		delta, err := strconv.ParseFloat(string(m.Args[1]), 64)
		if err != nil {
			return fmt.Errorf("apply INCRBYFLOAT: %w", err)
		}
		existing, ttl := s.resolveBytes(key)
		var cur float64
		if len(existing) > 0 {
			cur, err = strconv.ParseFloat(strings.TrimSpace(string(existing)), 64)
			if err != nil {
				return fmt.Errorf("apply INCRBYFLOAT: bad existing value: %w", err)
			}
		}
		s.items[key] = &memEntry{
			value:     []byte(strconv.FormatFloat(cur+delta, 'f', -1, 64)),
			valueType: ValueTypeBytes,
			expireAt:  ttl,
		}

	case "EXPIRE":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply EXPIRE: need >= 2 args, got %d", len(m.Args))
		}
		sec, err := strconv.ParseInt(string(m.Args[1]), 10, 64)
		if err != nil {
			return fmt.Errorf("apply EXPIRE: %w", err)
		}
		if e, ok := s.items[key]; ok {
			e.expireAt = time.Now().Add(time.Duration(sec) * time.Second).UnixNano()
		}

	case "PEXPIRE":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply PEXPIRE: need >= 2 args, got %d", len(m.Args))
		}
		ms, err := strconv.ParseInt(string(m.Args[1]), 10, 64)
		if err != nil {
			return fmt.Errorf("apply PEXPIRE: %w", err)
		}
		if e, ok := s.items[key]; ok {
			e.expireAt = time.Now().Add(time.Duration(ms) * time.Millisecond).UnixNano()
		}

	case "RENAME":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply RENAME: need >= 2 args, got %d", len(m.Args))
		}
		e, ok := s.items[key]
		if !ok {
			return nil
		}
		s.items[string(m.Args[1])] = e
		delete(s.items, key)

	case "FLUSHDB", "FLUSHALL":
		s.items = make(map[string]*memEntry)

	// --- hash ---

	case "HSET":
		if len(m.Args) < 3 || (len(m.Args)-1)%2 != 0 {
			return fmt.Errorf("apply HSET: need key + field-value pairs, got %d args", len(m.Args))
		}
		h, ttl := s.getHash(key)
		for i := 1; i+1 < len(m.Args); i += 2 {
			h[string(m.Args[i])] = string(m.Args[i+1])
		}
		s.items[key] = &memEntry{value: h, valueType: ValueTypeHash, expireAt: ttl}

	case "HDEL":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply HDEL: need >= 2 args, got %d", len(m.Args))
		}
		e, ok := s.items[key]
		if !ok {
			return nil
		}
		h, ok := e.value.(map[string]string)
		if !ok {
			return fmt.Errorf("apply HDEL: key %q is not a hash", key)
		}
		for _, f := range m.Args[1:] {
			delete(h, string(f))
		}
		if len(h) == 0 {
			delete(s.items, key)
		}

	// --- set ---

	case "SADD":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply SADD: need >= 2 args, got %d", len(m.Args))
		}
		st, ttl := s.getSet(key)
		for _, member := range m.Args[1:] {
			st[string(member)] = struct{}{}
		}
		s.items[key] = &memEntry{value: st, valueType: ValueTypeSet, expireAt: ttl}

	case "SREM":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply SREM: need >= 2 args, got %d", len(m.Args))
		}
		e, ok := s.items[key]
		if !ok {
			return nil
		}
		st, ok := e.value.(map[string]struct{})
		if !ok {
			return fmt.Errorf("apply SREM: key %q is not a set", key)
		}
		for _, member := range m.Args[1:] {
			delete(st, string(member))
		}
		if len(st) == 0 {
			delete(s.items, key)
		}

	case "SPOP":
		count := 1
		if len(m.Args) > 1 {
			count, _ = strconv.Atoi(string(m.Args[1]))
		}
		e, ok := s.items[key]
		if !ok {
			return nil
		}
		st, ok := e.value.(map[string]struct{})
		if !ok {
			return fmt.Errorf("apply SPOP: key %q is not a set", key)
		}
		i := 0
		for member := range st {
			if i >= count {
				break
			}
			delete(st, member)
			i++
		}
		if len(st) == 0 {
			delete(s.items, key)
		}

	// --- sorted set ---

	case "ZADD":
		if len(m.Args) < 3 {
			return fmt.Errorf("apply ZADD: need >= 3 args, got %d", len(m.Args))
		}
		z, ttl := s.getSortedSet(key)
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
			z[string(m.Args[i+1])] = score
		}
		s.items[key] = &memEntry{value: z, valueType: ValueTypeSortedSet, expireAt: ttl}

	case "ZREM":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply ZREM: need >= 2 args, got %d", len(m.Args))
		}
		e, ok := s.items[key]
		if !ok {
			return nil
		}
		z, ok := e.value.(map[string]float64)
		if !ok {
			return fmt.Errorf("apply ZREM: key %q is not a sorted set", key)
		}
		for _, member := range m.Args[1:] {
			delete(z, string(member))
		}
		if len(z) == 0 {
			delete(s.items, key)
		}

	// --- list ---

	case "LPUSH":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply LPUSH: need >= 2 args, got %d", len(m.Args))
		}
		l, ttl := s.getList(key)
		for _, v := range m.Args[1:] {
			l = append([]string{string(v)}, l...)
		}
		s.items[key] = &memEntry{value: l, valueType: ValueTypeList, expireAt: ttl}

	case "RPUSH":
		if len(m.Args) < 2 {
			return fmt.Errorf("apply RPUSH: need >= 2 args, got %d", len(m.Args))
		}
		l, ttl := s.getList(key)
		for _, v := range m.Args[1:] {
			l = append(l, string(v))
		}
		s.items[key] = &memEntry{value: l, valueType: ValueTypeList, expireAt: ttl}

	case "LPOP":
		return s.applyPop(key, m.Args, true)
	case "RPOP":
		return s.applyPop(key, m.Args, false)

	case "LSET":
		if len(m.Args) < 3 {
			return fmt.Errorf("apply LSET: need >= 3 args, got %d", len(m.Args))
		}
		e, ok := s.items[key]
		if !ok {
			return fmt.Errorf("apply LSET: key %q not found", key)
		}
		l, ok := e.value.([]string)
		if !ok {
			return fmt.Errorf("apply LSET: key %q is not a list", key)
		}
		idx, err := strconv.Atoi(string(m.Args[1]))
		if err != nil {
			return fmt.Errorf("apply LSET: %w", err)
		}
		if idx < 0 {
			idx = len(l) + idx
		}
		if idx < 0 || idx >= len(l) {
			return fmt.Errorf("apply LSET: index out of range")
		}
		l[idx] = string(m.Args[2])

	default:
		return fmt.Errorf("unknown mutation op: %s", m.Op)
	}
	return nil
}

// --- helpers ---

func (s *MemoryStore) resolveBytes(key string) ([]byte, int64) {
	e, ok := s.items[key]
	if !ok {
		return nil, 0
	}
	switch v := e.value.(type) {
	case []byte:
		return v, e.expireAt
	case string:
		return []byte(v), e.expireAt
	}
	return nil, e.expireAt
}

func (s *MemoryStore) applyIncrBy(key string, delta int64) error {
	existing, ttl := s.resolveBytes(key)
	var cur int64
	if len(existing) > 0 {
		var err error
		cur, err = strconv.ParseInt(strings.TrimSpace(string(existing)), 10, 64)
		if err != nil {
			return fmt.Errorf("apply INCR/DECRBY %q: %w", key, err)
		}
	}
	s.items[key] = &memEntry{
		value:     []byte(strconv.FormatInt(cur+delta, 10)),
		valueType: ValueTypeBytes,
		expireAt:  ttl,
	}
	return nil
}

func (s *MemoryStore) applyPop(key string, args [][]byte, left bool) error {
	count := 1
	if len(args) > 1 {
		count, _ = strconv.Atoi(string(args[1]))
	}
	e, ok := s.items[key]
	if !ok {
		return nil
	}
	l, ok := e.value.([]string)
	if !ok {
		return fmt.Errorf("apply L/RPOP: key %q is not a list", key)
	}
	for i := 0; i < count && len(l) > 0; i++ {
		if left {
			l = l[1:]
		} else {
			l = l[:len(l)-1]
		}
	}
	if len(l) == 0 {
		delete(s.items, key)
	} else {
		e.value = l
	}
	return nil
}

func (s *MemoryStore) getHash(key string) (map[string]string, int64) {
	e, ok := s.items[key]
	if !ok {
		return make(map[string]string), 0
	}
	if h, ok := e.value.(map[string]string); ok {
		return h, e.expireAt
	}
	return make(map[string]string), e.expireAt
}

func (s *MemoryStore) getSet(key string) (map[string]struct{}, int64) {
	e, ok := s.items[key]
	if !ok {
		return make(map[string]struct{}), 0
	}
	if st, ok := e.value.(map[string]struct{}); ok {
		return st, e.expireAt
	}
	return make(map[string]struct{}), e.expireAt
}

func (s *MemoryStore) getSortedSet(key string) (map[string]float64, int64) {
	e, ok := s.items[key]
	if !ok {
		return make(map[string]float64), 0
	}
	if z, ok := e.value.(map[string]float64); ok {
		return z, e.expireAt
	}
	return make(map[string]float64), e.expireAt
}

func (s *MemoryStore) getList(key string) ([]string, int64) {
	e, ok := s.items[key]
	if !ok {
		return nil, 0
	}
	if l, ok := e.value.([]string); ok {
		return l, e.expireAt
	}
	return nil, e.expireAt
}

func parseExpiration(opts [][]byte) int64 {
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
