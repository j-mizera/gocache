package cache

import (
	"container/list"
	"context"
	"errors"
	"gocache/api/logger"
	"strings"
	"sync"
	"time"
)

// ErrOutOfMemory is returned by RawSet when the memory limit is reached and
// the eviction policy is EvictionNone.
var ErrOutOfMemory = errors.New("OOM command not allowed when used memory > 'maxmemory'")

// EvictionPolicy defines what happens when the memory limit is exceeded.
type EvictionPolicy int

const (
	EvictionNone EvictionPolicy = iota // noeviction: reject writes
	EvictionLRU                        // allkeys-lru: evict least recently used key
)

// ParseEvictionPolicy converts a config string to an EvictionPolicy.
// "lru" and "allkeys-lru" map to EvictionLRU; anything else is EvictionNone.
func ParseEvictionPolicy(s string) EvictionPolicy {
	switch strings.ToLower(s) {
	case "lru", "allkeys-lru":
		return EvictionLRU
	default:
		return EvictionNone
	}
}

// entryOverhead is a conservative per-entry constant (bytes) that accounts
// for map bucket amortization, the Entry struct, and the LRU list node.
const entryOverhead = 128

// bytesPerMB converts a megabyte limit to bytes for the cache's byte budget.
const bytesPerMB int64 = 1024 * 1024

type ValueState int
type ValueType int

const (
	ValuePresent  ValueState = 0
	ValueNoExpire ValueState = -1
	ValueAbsent   ValueState = -2
	ValueExpired  ValueState = -3
)

const (
	ObjTypeBytes ValueType = iota
	ObjTypeList
	ObjTypeHash
	ObjTypeSet
	ObjTypeSortedSet
)

// Entry is the stored form of every cached value after Phase 1 of the
// slab-allocator plan. Value always holds a byte-encoded payload; the shape
// depends on ValueType (see encoding.go for the layouts). The on-storage
// size is exactly `entryOverhead + len(key) + len(Value)` — estimateSize
// is gone.
type Entry struct {
	ValueType    ValueType `json:"value_type"`
	Value        []byte    `json:"value"`
	LastAccessed time.Time `json:"-"`
}

type Cache struct {
	mu             sync.RWMutex
	items          map[string]*Entry
	ttl            map[string]int64
	maxBytes       int64 // 0 = unlimited
	usedBytes      int64
	evictionPolicy EvictionPolicy
	lruList        *list.List // front = most recently used
	lruMap         map[string]*list.Element
	OnMutate       func(key string) // called after a key is set or deleted (for WATCH)
	OnMutateAll    func()           // called when all keys are invalidated (FLUSHDB)
}

func New() *Cache {
	return newCache(0, EvictionLRU)
}

func NewWithConfig(maxMemoryMB int64, policy EvictionPolicy) *Cache {
	var maxBytes int64
	if maxMemoryMB > 0 {
		maxBytes = maxMemoryMB * bytesPerMB
	}
	return newCache(maxBytes, policy)
}

// NewWithBytes creates a cache with a raw byte limit. Intended for testing.
func NewWithBytes(maxBytes int64, policy EvictionPolicy) *Cache {
	return newCache(maxBytes, policy)
}

func newCache(maxBytes int64, policy EvictionPolicy) *Cache {
	return &Cache{
		items:          make(map[string]*Entry),
		ttl:            make(map[string]int64),
		maxBytes:       maxBytes,
		evictionPolicy: policy,
		lruList:        list.New(),
		lruMap:         make(map[string]*list.Element),
	}
}

// entrySize returns the storage cost (header overhead + key + encoded value)
// for a byte-encoded entry. Exact: this replaces the reflective estimateSize
// of the pre-Phase-1 implementation.
func entrySize(key string, value []byte) int64 {
	return int64(entryOverhead) + int64(len(key)) + int64(len(value))
}

func (c *Cache) Lock() {
	c.mu.Lock()
}

func (c *Cache) Unlock() {
	c.mu.Unlock()
}

func (c *Cache) RLock() {
	c.mu.RLock()
}

func (c *Cache) RUnlock() {
	c.mu.RUnlock()
}

// SetMemoryLimit updates the memory limit and eviction policy at runtime.
// Safe to call from any goroutine. ctx carries the operation (e.g. config
// reload) for log correlation.
func (c *Cache) SetMemoryLimit(ctx context.Context, maxMemoryMB int64, policy EvictionPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if maxMemoryMB > 0 {
		c.maxBytes = maxMemoryMB * bytesPerMB
	} else {
		c.maxBytes = 0
	}
	c.evictionPolicy = policy
	logger.Info(ctx).Int64("maxBytes", c.maxBytes).Str("policy", c.EvictionPolicyString()).Msg("memory limit updated")

	// Evict if the new limit is below current usage.
	if c.maxBytes > 0 && c.usedBytes > c.maxBytes && c.evictionPolicy == EvictionLRU {
		c.evictLRU(ctx, 0)
	}
}

// Len returns the number of keys in the cache.
func (c *Cache) Len() int {
	return len(c.items)
}

// EvictionPolicyString returns the eviction policy as a human-readable string.
func (c *Cache) EvictionPolicyString() string {
	switch c.evictionPolicy {
	case EvictionLRU:
		return "allkeys-lru"
	default:
		return "noeviction"
	}
}

// UsedBytes returns the estimated number of bytes currently used by all entries.
func (c *Cache) UsedBytes() int64 {
	return c.usedBytes
}

// MaxBytes returns the configured memory limit in bytes (0 = unlimited).
func (c *Cache) MaxBytes() int64 {
	return c.maxBytes
}

// RawSetTyped stores a byte-encoded value under the given ValueType,
// enforcing the memory limit. It evicts LRU entries as needed
// (EvictionLRU) or returns ErrOutOfMemory (EvictionNone) when the limit
// would be exceeded. Must be called while holding the cache write lock.
// ctx carries the operation (command, cleanup, etc.) for log correlation.
func (c *Cache) RawSetTyped(ctx context.Context, key string, vt ValueType, value []byte, expiration int64) error {
	if c.maxBytes > 0 {
		newSize := entrySize(key, value)
		oldSize := c.oldEntrySize(key)
		delta := newSize - oldSize
		if delta > 0 && c.usedBytes+delta > c.maxBytes {
			switch c.evictionPolicy {
			case EvictionLRU:
				c.evictLRU(ctx, delta)
			case EvictionNone:
				logger.Warn(ctx).Str("key", key).Int64("usedBytes", c.usedBytes).Int64("maxBytes", c.maxBytes).Msg("write rejected, out of memory")
				return ErrOutOfMemory
			}
		}
	}
	c.setInternal(key, vt, value, expiration, false)
	return nil
}

// RawLoad stores a byte-encoded entry, bypassing the memory limit check.
// Intended for snapshot loading only. Still maintains LRU and accounting.
// Must be called while holding the cache write lock. The OnMutate callback
// is suppressed since this is a bulk load, not a client mutation.
func (c *Cache) RawLoad(key string, vt ValueType, value []byte, expiration int64) {
	c.setInternal(key, vt, value, expiration, true)
}

// setInternal installs a byte-encoded entry under the given ValueType,
// updating LRU and usedBytes bookkeeping. When suppressMutate is true the
// OnMutate callback is not invoked (used by snapshot loads that bulk-
// populate without triggering WATCH dirty marks).
func (c *Cache) setInternal(key string, vt ValueType, value []byte, expiration int64, suppressMutate bool) {
	newSize := entrySize(key, value)
	oldSize := c.oldEntrySize(key)
	c.usedBytes += newSize - oldSize

	if elem, ok := c.lruMap[key]; ok {
		c.lruList.MoveToFront(elem)
	} else {
		elem = c.lruList.PushFront(key)
		c.lruMap[key] = elem
	}

	c.items[key] = &Entry{
		ValueType:    vt,
		Value:        value,
		LastAccessed: time.Now(),
	}
	if expiration > 0 {
		c.ttl[key] = expiration
	} else {
		delete(c.ttl, key)
	}
	if !suppressMutate && c.OnMutate != nil {
		c.OnMutate(key)
	}
}

// oldEntrySize returns the storage cost of whatever is currently at key, or
// 0 if the key is unset. Used to compute the delta during a replace.
func (c *Cache) oldEntrySize(key string) int64 {
	if existing, ok := c.items[key]; ok {
		return entrySize(key, existing.Value)
	}
	return 0
}

// evictLRU removes least recently used entries until delta bytes can be
// accommodated within the memory limit.
func (c *Cache) evictLRU(ctx context.Context, delta int64) {
	for c.maxBytes > 0 && c.usedBytes+delta > c.maxBytes {
		elem := c.lruList.Back()
		if elem == nil {
			break
		}
		evictKey := elem.Value.(string)
		logger.Debug(ctx).Str("key", evictKey).Msg("lru eviction")
		c.delete(evictKey)
	}
}

// RawGet returns the entry for key, updating its LRU position.
// Must be called while holding the cache lock.
func (c *Cache) RawGet(key string) (*Entry, bool) {
	entry, found := c.items[key]
	if found {
		entry.LastAccessed = time.Now()
		if elem, ok := c.lruMap[key]; ok {
			c.lruList.MoveToFront(elem)
		}
	}
	return entry, found
}

func (c *Cache) RawDelete(key string) {
	c.delete(key)
}

// TTLInternal returns the remaining TTL and a ValueState for key.
//
// States:
//
//	ValuePresent   — key exists with a future expiration (ttl > 0)
//	ValueExpired   — key has a TTL that has already passed (caller should
//	                 lazyExpire to clean it up)
//	ValueNoExpire  — key exists but no TTL is set
//	ValueAbsent    — key does not exist in the cache
//
// Callers that need to distinguish "missing" from "no TTL" (TTL/PTTL) can
// rely on ValueAbsent vs ValueNoExpire directly. Must be called while
// holding the cache read lock.
func (c *Cache) TTLInternal(key string) (time.Duration, ValueState) {
	if expiration, found := c.ttl[key]; found {
		expirationTime := time.Unix(0, expiration)
		if expirationTime.Before(time.Now()) {
			return 0, ValueExpired
		}
		return time.Until(expirationTime), ValuePresent
	}
	if _, exists := c.items[key]; exists {
		return 0, ValueNoExpire
	}
	return 0, ValueAbsent
}

func (c *Cache) delete(key string) {
	if existing, ok := c.items[key]; ok {
		c.usedBytes -= entrySize(key, existing.Value)
	}
	if elem, ok := c.lruMap[key]; ok {
		c.lruList.Remove(elem)
		delete(c.lruMap, key)
	}
	delete(c.ttl, key)
	delete(c.items, key)
	if c.OnMutate != nil {
		c.OnMutate(key)
	}
}

func (c *Cache) Range(fn func(key string, entry *Entry, expiration int64) bool) {
	for key, entry := range c.items {
		if !fn(key, entry, c.ttl[key]) {
			break
		}
	}
}

// RawTTL returns the raw expiration timestamp in nanoseconds for the given key.
// Returns 0 if the key has no TTL set.
func (c *Cache) RawTTL(key string) int64 {
	return c.ttl[key]
}

func (c *Cache) Clear(ctx context.Context) {
	logger.Info(ctx).Int("items", len(c.items)).Msg("cache cleared")
	c.items = make(map[string]*Entry)
	c.ttl = make(map[string]int64)
	c.lruList.Init()
	c.lruMap = make(map[string]*list.Element)
	c.usedBytes = 0
	if c.OnMutateAll != nil {
		c.OnMutateAll()
	}
}
