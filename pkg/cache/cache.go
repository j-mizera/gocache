package cache

import (
	"container/list"
	"context"
	"errors"
	"gocache/api/logger"
	"gocache/pkg/cache/slab"
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

// Encoding distinguishes the two physical shapes a collection can take.
// Small collections live as EncPacked — a flat byte buffer that handlers
// mutate in place via pkg/cache/packed. Large ones live as EncNative — the
// existing Go-map / Go-slice / *SortedSet shapes. Strings are always
// EncPacked ([]byte); this field is orthogonal to ValueType.
type Encoding uint8

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

const (
	EncNative Encoding = iota // Go-native map/slice/*SortedSet
	EncPacked                 // flat []byte (pkg/cache/packed layouts)
)

type Entry struct {
	ValueType ValueType `json:"value_type"`
	Encoding  Encoding  `json:"encoding"`
	// Value holds the native Go shape for EncNative entries (map/slice/
	// *SortedSet). For EncPacked entries Value is nil and the bytes live
	// inside the slab allocator — resolve via Cache.ResolvePacked.
	Value any `json:"value"`
	// Ptr identifies the slab-allocated byte region for EncPacked entries.
	// Zero (NilPointer) for EncNative entries.
	Ptr          slab.SlabPointer `json:"-"`
	LastAccessed time.Time        `json:"-"`
}

// PackedThresholds controls when a collection is promoted from its packed
// (byte-encoded) form to its native Go shape. Handlers read these to
// decide after each mutation whether the post-op state crosses a limit.
// Defaults mirror Valkey 8 and live in pkg/config.
type PackedThresholds struct {
	HashMaxEntries int // hash promotes when count > HashMaxEntries
	HashMaxValue   int // hash promotes when any value length > HashMaxValue
	SetMaxEntries  int
	SetMaxValue    int
	ZSetMaxEntries int
	ZSetMaxValue   int
	ListMaxBytes   int // list promotes when encoded buffer > ListMaxBytes
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
	sizes          map[string]int64
	packed         PackedThresholds
	slabs          *slab.Allocator  // backs EncPacked byte payloads
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
		sizes:          make(map[string]int64),
		slabs:          slab.NewAllocator(),
		// Defaults mirror Valkey 8 (src/config.c). SetPackedThresholds
		// overrides them with config values at boot.
		packed: PackedThresholds{
			HashMaxEntries: 512,
			HashMaxValue:   64,
			SetMaxEntries:  128,
			SetMaxValue:    64,
			ZSetMaxEntries: 128,
			ZSetMaxValue:   64,
			ListMaxBytes:   8192,
		},
	}
}

// ResolvePacked returns a zero-copy view of an EncPacked entry's bytes. The
// returned slice aliases the slab allocator's backing storage; callers must
// not retain it past the current cache-lock hold.
func (c *Cache) ResolvePacked(e *Entry) []byte {
	if e == nil || e.Encoding != EncPacked || e.Ptr.IsNil() {
		return nil
	}
	return c.slabs.Read(e.Ptr)
}

// SlabStats returns a point-in-time snapshot of slab allocator accounting.
// Exposed for INFO / diagnostics. Must be called under a read lock.
func (c *Cache) SlabStats() slab.Stats {
	return c.slabs.Stats()
}

// SetExpiration updates only the TTL for an existing key, leaving the value
// in place. Returns true if the key existed and the expiration was applied;
// false if the key was absent. Must be called under the write lock.
func (c *Cache) SetExpiration(key string, expiration int64) bool {
	if _, ok := c.items[key]; !ok {
		return false
	}
	if expiration > 0 {
		c.ttl[key] = expiration
	} else {
		delete(c.ttl, key)
	}
	if c.OnMutate != nil {
		c.OnMutate(key)
	}
	return true
}

// Rename moves src's entry to dst in place, preserving the slab allocation
// and LRU node. Any existing dst entry is freed. The dst TTL is set to
// newExpiration (0 = no TTL). Returns false if src is absent. Must be called
// under the write lock.
func (c *Cache) Rename(src, dst string, newExpiration int64) bool {
	entry, ok := c.items[src]
	if !ok {
		return false
	}
	if _, exists := c.items[dst]; exists {
		c.delete(dst)
	}

	c.items[dst] = entry
	if sz, ok := c.sizes[src]; ok {
		c.sizes[dst] = sz
		delete(c.sizes, src)
	}
	if elem, ok := c.lruMap[src]; ok {
		elem.Value = dst
		c.lruMap[dst] = elem
		delete(c.lruMap, src)
	}
	delete(c.items, src)
	delete(c.ttl, src)

	if newExpiration > 0 {
		c.ttl[dst] = newExpiration
	} else {
		delete(c.ttl, dst)
	}

	if c.OnMutate != nil {
		c.OnMutate(src)
		c.OnMutate(dst)
	}
	return true
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

// PackedThresholds returns the current hybrid-encoding thresholds.
func (c *Cache) PackedThresholds() PackedThresholds {
	return c.packed
}

// SetPackedThresholds replaces the hybrid-encoding thresholds. Safe to call
// at boot or during config hot-reload. Existing entries are not migrated;
// they stay at whatever encoding they were written with until their next
// mutation.
func (c *Cache) SetPackedThresholds(t PackedThresholds) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.packed = t
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

// RawSet stores a key with the given value and expiration, enforcing the
// memory limit. It evicts LRU entries as needed (EvictionLRU) or returns
// ErrOutOfMemory (EvictionNone) when the limit would be exceeded.
// Must be called while holding the cache write lock. ctx carries the
// operation (command, cleanup, etc.) for log correlation.
func (c *Cache) RawSet(ctx context.Context, key string, value any, expiration int64) error {
	if c.maxBytes > 0 {
		newSize := estimateSize(key, value)
		oldSize := c.sizes[key]
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
	c.setInternal(key, value, expiration, false)
	return nil
}

// RawLoad stores a key-value pair, bypassing the memory limit check.
// Intended for snapshot loading only. Still maintains LRU and size tracking.
// Must be called while holding the cache write lock. The OnMutate callback
// is suppressed since this is a bulk load, not a client mutation — the
// previous implementation stashed and restored c.OnMutate around the call,
// which left the callback nil if setInternal ever panicked.
func (c *Cache) RawLoad(key string, value any, expiration int64) {
	c.setInternal(key, value, expiration, true)
}

// RawSetPacked stores a packed byte-encoded value for the given ValueType.
// The buffer layout must match pkg/cache/packed for that type. Enforces
// the memory limit (evicting or returning ErrOutOfMemory as configured).
// Must be called while holding the cache write lock.
func (c *Cache) RawSetPacked(ctx context.Context, key string, vt ValueType, buf []byte, expiration int64) error {
	if c.maxBytes > 0 {
		newSize := estimateBytesSize(key, buf)
		oldSize := c.sizes[key]
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
	c.setPackedInternal(key, vt, buf, expiration, false)
	return nil
}

// RawLoadPacked stores a packed byte buffer, bypassing the memory limit
// check. Snapshot-loading uses this for entries with Encoding == EncPacked.
func (c *Cache) RawLoadPacked(key string, vt ValueType, buf []byte, expiration int64) {
	c.setPackedInternal(key, vt, buf, expiration, true)
}

// setPackedInternal performs the raw packed-storage operation. The byte
// payload is copied into a slab slot and the returned SlabPointer is stored
// on Entry.Ptr. Existing packed entries reuse their current slot when the
// slab class capacity allows, avoiding churn on same-size or shrinking
// updates; otherwise the old slot is freed and a fresh one is allocated.
func (c *Cache) setPackedInternal(key string, vt ValueType, buf []byte, expiration int64, suppressMutate bool) {
	newSize := estimateBytesSize(key, buf)
	oldSize := c.sizes[key]
	c.usedBytes += newSize - oldSize
	c.sizes[key] = newSize

	if elem, ok := c.lruMap[key]; ok {
		c.lruList.MoveToFront(elem)
	} else {
		elem = c.lruList.PushFront(key)
		c.lruMap[key] = elem
	}

	var ptr slab.SlabPointer
	if prev, ok := c.items[key]; ok && prev.Encoding == EncPacked && !prev.Ptr.IsNil() &&
		c.slabs.Capacity(prev.Ptr) >= uint32(len(buf)) {
		// Reuse the existing slot — same or shrinking payload fits.
		ptr = prev.Ptr
	} else {
		// Free the old slot if it existed as a packed entry.
		if prev, ok := c.items[key]; ok && prev.Encoding == EncPacked && !prev.Ptr.IsNil() {
			c.slabs.Free(prev.Ptr)
		}
		ptr = c.slabs.Alloc(uint32(len(buf)))
	}
	c.slabs.Write(ptr, buf)

	c.items[key] = &Entry{
		ValueType:    vt,
		Encoding:     EncPacked,
		Ptr:          ptr,
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

// estimateBytesSize charges the exact encoded size of a packed []byte value
// plus the key and per-entry overhead. The slab allocator (Phase 2) will
// supersede this with a slab-aware accounting that knows the exact slab
// page and fragment cost.
func estimateBytesSize(key string, buf []byte) int64 {
	return int64(entryOverhead) + int64(len(key)) + int64(len(buf))
}

// setInternal performs the raw storage operation, updating LRU and size tracking.
// When suppressMutate is true the OnMutate callback is not invoked (used by
// snapshot loads that bulk-populate without triggering WATCH dirty marks).
//
// Values of []byte / string are routed to the slab-backed packed path so
// string entries never allocate a GC-tracked payload. Native container shapes
// stay in Entry.Value as before.
func (c *Cache) setInternal(key string, value any, expiration int64, suppressMutate bool) {
	switch v := value.(type) {
	case []byte:
		c.setPackedInternal(key, ObjTypeBytes, v, expiration, suppressMutate)
		return
	case string:
		c.setPackedInternal(key, ObjTypeBytes, []byte(v), expiration, suppressMutate)
		return
	}

	newSize := estimateSize(key, value)
	oldSize := c.sizes[key]
	c.usedBytes += newSize - oldSize
	c.sizes[key] = newSize

	if elem, ok := c.lruMap[key]; ok {
		c.lruList.MoveToFront(elem)
	} else {
		elem = c.lruList.PushFront(key)
		c.lruMap[key] = elem
	}

	// If we're overwriting an existing packed entry with a native shape,
	// free its slab slot first.
	if prev, ok := c.items[key]; ok && prev.Encoding == EncPacked && !prev.Ptr.IsNil() {
		c.slabs.Free(prev.Ptr)
	}

	valueType := ObjTypeBytes
	switch value.(type) {
	case []string:
		valueType = ObjTypeList
	case map[string]string:
		valueType = ObjTypeHash
	case map[string]struct{}:
		valueType = ObjTypeSet
	case *SortedSet:
		valueType = ObjTypeSortedSet
	}

	c.items[key] = &Entry{
		ValueType:    valueType,
		Encoding:     EncNative,
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
	if entry, ok := c.items[key]; ok && entry.Encoding == EncPacked && !entry.Ptr.IsNil() {
		c.slabs.Free(entry.Ptr)
	}
	if sz, ok := c.sizes[key]; ok {
		c.usedBytes -= sz
		delete(c.sizes, key)
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
	c.sizes = make(map[string]int64)
	c.usedBytes = 0
	// Drop the entire slab arena — faster than walking entries to Free().
	c.slabs = slab.NewAllocator()
	if c.OnMutateAll != nil {
		c.OnMutateAll()
	}
}

// estimateSize returns an approximate memory usage in bytes for a key-value pair.
// Note: []byte values are charged their exact byte length — this is the
// target invariant for all types once Phase 1 completes; the string / []string
// / map / *SortedSet branches remain for the handlers that haven't migrated
// to byte-encoded values yet.
func estimateSize(key string, value any) int64 {
	size := int64(entryOverhead) + int64(len(key))
	switch v := value.(type) {
	case []byte:
		size += int64(len(v))
	case string:
		size += int64(len(v))
	case []string:
		for _, s := range v {
			size += int64(len(s)) + 16
		}
	case map[string]string:
		for k, val := range v {
			size += int64(len(k)) + int64(len(val)) + 32
		}
	case map[string]struct{}:
		for k := range v {
			size += int64(len(k)) + 16
		}
	case *SortedSet:
		size += v.EstimateSize()
	}
	return size
}
