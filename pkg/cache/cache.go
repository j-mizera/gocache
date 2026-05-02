package cache

import (
	"context"
	"errors"
	"gocache/api/logger"
	"gocache/pkg/cache/slab"
	"strings"
	"sync"
	"sync/atomic"
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
// for map bucket amortization + slab slot overhead.
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

// Entry is a value-type snapshot of a cache entry. It is assembled on demand
// by RawGet / Range from the slab meta and the native-value sidecar; callers
// never store *Entry in the cache. The items map stores bare SlabPointers,
// which are inert to the GC.
type Entry struct {
	ValueType ValueType
	Encoding  Encoding
	// Value holds the native Go shape for EncNative entries (map/slice/
	// *SortedSet). Nil for EncPacked entries — resolve bytes via
	// Cache.ResolvePacked.
	Value any
	// Ptr identifies the slab slot backing this entry. Slot hosts the
	// SlotMeta (LRU pointers, last-access, value-type, encoding) plus
	// the packed bytes (for EncPacked) or is otherwise unused (EncNative).
	Ptr slab.SlabPointer
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
	items          map[string]slab.SlabPointer
	nativeValues   map[slab.SlabPointer]any // populated only for EncNative entries
	maxBytes       int64                    // 0 = unlimited
	usedBytes      int64
	evictionPolicy EvictionPolicy
	// LRU list is threaded through slab.SlotMeta's LRUPrev/LRUNext. The
	// head is the most-recently used entry; tail is the eviction target.
	// A NilPointer head/tail means the list is empty.
	lruHead slab.SlabPointer
	lruTail slab.SlabPointer
	// keysBySlot maps a slab pointer back to the key that owns it. Needed
	// during eviction: the slot meta knows its LRU neighbours but not the
	// key. The uint64 key is inert to the GC; only the string value
	// contributes a per-entry pointer (shared backing array with items).
	keysBySlot  map[slab.SlabPointer]string
	packed      PackedThresholds
	slabs       *slab.Allocator  // backs EncPacked byte payloads + per-entry meta
	OnMutate    func(key string) // called after a key is set or deleted (for WATCH)
	OnMutateAll func()           // called when all keys are invalidated (FLUSHDB)
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
		items:          make(map[string]slab.SlabPointer),
		nativeValues:   make(map[slab.SlabPointer]any),
		maxBytes:       maxBytes,
		evictionPolicy: policy,
		keysBySlot:     make(map[slab.SlabPointer]string),
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

// entryFromSlot reconstructs an Entry value from a slab slot. Caller must
// ensure ptr is live (present in keysBySlot).
func (c *Cache) entryFromSlot(ptr slab.SlabPointer) Entry {
	meta := c.slabs.Meta(ptr)
	enc := Encoding(meta.Encoding)
	var value any
	if enc == EncNative {
		value = c.nativeValues[ptr]
	}
	return Entry{
		ValueType: ValueType(meta.ValueType),
		Encoding:  enc,
		Value:     value,
		Ptr:       ptr,
	}
}

// LastAccess returns the wall-clock time of the last access to this entry,
// sourced from the slab meta's LastAccessNs. Zero time means "unknown"
// (never accessed or slot freed).
//
// Read with atomic.LoadInt64 because RawGet (potentially under RLock-only)
// updates this field via atomic.StoreInt64.
func (c *Cache) LastAccess(e Entry) time.Time {
	if e.Ptr.IsNil() {
		return time.Time{}
	}
	ns := atomic.LoadInt64(&c.slabs.Meta(e.Ptr).LastAccessNs)
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// ResolvePacked returns a zero-copy view of an EncPacked entry's bytes. The
// returned slice aliases the slab allocator's backing storage; callers must
// not retain it past the current cache-lock hold.
func (c *Cache) ResolvePacked(e Entry) []byte {
	if e.Encoding != EncPacked || e.Ptr.IsNil() {
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
	ptr, ok := c.items[key]
	if !ok {
		return false
	}
	if expiration > 0 {
		c.slabs.Meta(ptr).ExpirationNs = expiration
	} else {
		c.slabs.Meta(ptr).ExpirationNs = 0
	}
	if c.OnMutate != nil {
		c.OnMutate(key)
	}
	return true
}

// Rename moves src's entry to dst in place, preserving the slab allocation
// and LRU position. Any existing dst entry is freed. The dst TTL is set to
// newExpiration (0 = no TTL). Returns false if src is absent. usedBytes is
// re-charged because the key string component of the per-entry cost changes.
// Must be called under the write lock.
func (c *Cache) Rename(src, dst string, newExpiration int64) bool {
	ptr, ok := c.items[src]
	if !ok {
		return false
	}
	if _, exists := c.items[dst]; exists {
		c.delete(dst)
	}

	c.usedBytes += int64(len(dst)) - int64(len(src))

	c.items[dst] = ptr
	if !ptr.IsNil() {
		c.keysBySlot[ptr] = dst
		if newExpiration > 0 {
			c.slabs.Meta(ptr).ExpirationNs = newExpiration
		} else {
			c.slabs.Meta(ptr).ExpirationNs = 0
		}
	}
	delete(c.items, src)

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
		oldSize := c.keySize(key)
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
// Must be called while holding the cache write lock.
func (c *Cache) RawLoad(key string, value any, expiration int64) {
	c.setInternal(key, value, expiration, true)
}

// RawSetNativeWithSize stores a Go-native collection value at key without
// walking it to compute size. byteSize is the payload byte cost (the same
// number estimateSize would return minus per-entry overhead and len(key));
// callers track it incrementally as they mutate the underlying
// map/slice. This is the O(1) write path used by HSET/LPUSH/SADD/ZADD on
// promoted entries. Closes #23.
//
// Must be called while holding the cache write lock.
func (c *Cache) RawSetNativeWithSize(ctx context.Context, key string, value any, byteSize int64, expiration int64) error {
	if c.maxBytes > 0 {
		newSize := int64(entryOverhead) + int64(len(key)) + byteSize
		oldSize := c.keySize(key)
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
	c.setNativeInternal(key, value, valueTypeOf(value), byteSize, expiration, false)
	return nil
}

// NativeSize returns the cached byte-size of an EncNative entry. Returns 0
// for absent keys or EncPacked entries (the packed size lives in the slab
// allocator's class capacity). Handlers use this as the seed value for
// incremental delta tracking — see hsetNative + finishHsetNative.
//
// Must be called while holding the cache read or write lock.
func (c *Cache) NativeSize(key string) int64 {
	ptr, ok := c.items[key]
	if !ok || ptr.IsNil() {
		return 0
	}
	return int64(c.slabs.Meta(ptr).NativeSize)
}

// RawSetPacked stores a packed byte-encoded value for the given ValueType.
// The buffer layout must match pkg/cache/packed for that type. Enforces
// the memory limit (evicting or returning ErrOutOfMemory as configured).
// Must be called while holding the cache write lock.
func (c *Cache) RawSetPacked(ctx context.Context, key string, vt ValueType, buf []byte, expiration int64) error {
	if c.maxBytes > 0 {
		newSize := estimateBytesSize(key, buf)
		oldSize := c.keySize(key)
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

// LRU helpers --------------------------------------------------------------
//
// The LRU list is doubly linked via LRUPrev/LRUNext fields inside each
// slab slot's SlotMeta. Head is the most-recently used; tail is the next
// eviction candidate. A NilPointer head/tail means the list is empty.

func (c *Cache) lruPushFront(ptr slab.SlabPointer) {
	meta := c.slabs.Meta(ptr)
	meta.LRUPrev = slab.NilPointer
	meta.LRUNext = c.lruHead
	if !c.lruHead.IsNil() {
		c.slabs.Meta(c.lruHead).LRUPrev = ptr
	}
	c.lruHead = ptr
	if c.lruTail.IsNil() {
		c.lruTail = ptr
	}
	// Atomic store so RLock-only readers (RawGet) can race-safely store the
	// same field via atomic.StoreInt64.
	atomic.StoreInt64(&meta.LastAccessNs, time.Now().UnixNano())
}

func (c *Cache) lruRemove(ptr slab.SlabPointer) {
	meta := c.slabs.Meta(ptr)
	prev := meta.LRUPrev
	next := meta.LRUNext
	if !prev.IsNil() {
		c.slabs.Meta(prev).LRUNext = next
	} else {
		c.lruHead = next
	}
	if !next.IsNil() {
		c.slabs.Meta(next).LRUPrev = prev
	} else {
		c.lruTail = prev
	}
	meta.LRUPrev = slab.NilPointer
	meta.LRUNext = slab.NilPointer
}

func (c *Cache) lruMoveToFront(ptr slab.SlabPointer) {
	if c.lruHead == ptr {
		atomic.StoreInt64(&c.slabs.Meta(ptr).LastAccessNs, time.Now().UnixNano())
		return
	}
	c.lruRemove(ptr)
	c.lruPushFront(ptr)
}

// setPackedInternal performs the raw packed-storage operation. The byte
// payload is copied into a slab slot; ValueType + Encoding + LRU state +
// TTL live in that slot's SlotMeta. Existing packed entries reuse the
// current slot when the class capacity fits; otherwise old is freed, new
// is allocated.
func (c *Cache) setPackedInternal(key string, vt ValueType, buf []byte, expiration int64, suppressMutate bool) {
	newSize := estimateBytesSize(key, buf)
	oldSize := c.keySize(key)
	c.usedBytes += newSize - oldSize

	prevPtr, had := c.items[key]
	var ptr slab.SlabPointer

	switch {
	case had && !prevPtr.IsNil() && c.slabs.Capacity(prevPtr) >= uint32(len(buf)) &&
		Encoding(c.slabs.Meta(prevPtr).Encoding) == EncPacked:
		// Same slot fits and was already packed: reuse in place.
		ptr = prevPtr
		c.slabs.Write(ptr, buf)
		c.lruMoveToFront(ptr)
	default:
		// New slot needed. Unlink the old LRU node and free the old slot.
		if had && !prevPtr.IsNil() {
			c.lruRemove(prevPtr)
			delete(c.keysBySlot, prevPtr)
			delete(c.nativeValues, prevPtr)
			c.slabs.Free(prevPtr)
		}
		ptr = c.slabs.Alloc(uint32(len(buf)))
		c.slabs.Write(ptr, buf)
		c.keysBySlot[ptr] = key
		c.lruPushFront(ptr)
	}

	meta := c.slabs.Meta(ptr)
	meta.ValueType = uint8(vt)
	meta.Encoding = uint8(EncPacked)
	if expiration > 0 {
		meta.ExpirationNs = expiration
	} else {
		meta.ExpirationNs = 0
	}

	c.items[key] = ptr
	if !suppressMutate && c.OnMutate != nil {
		c.OnMutate(key)
	}
}

// estimateBytesSize charges the exact encoded size of a packed []byte value
// plus the key and per-entry overhead.
func estimateBytesSize(key string, buf []byte) int64 {
	return int64(entryOverhead) + int64(len(key)) + int64(len(buf))
}

// setInternal performs the raw storage operation, updating LRU and size
// tracking. Values of []byte / string route to the slab-backed packed path.
// Native container shapes are stored in the nativeValues sidecar keyed by
// their slab slot; the slot's data region is unused but hosts the LRU meta.
//
// estimateSize walks the value (O(N) for collections); callers that already
// know the size should use setNativeInternalWithSize to skip the walk.
func (c *Cache) setInternal(key string, value any, expiration int64, suppressMutate bool) {
	switch v := value.(type) {
	case []byte:
		c.setPackedInternal(key, ObjTypeBytes, v, expiration, suppressMutate)
		return
	case string:
		c.setPackedInternal(key, ObjTypeBytes, []byte(v), expiration, suppressMutate)
		return
	}
	c.setNativeInternal(key, value, valueTypeOf(value), nativePayloadSize(key, value), expiration, suppressMutate)
}

// setNativeInternal stores a Go-native value at key. byteSize is the
// caller-provided payload size (bytes the entry costs minus per-entry
// overhead and the key length). It's recorded in the slot's SlotMeta so
// chargedSize / keySize never walk the value to compute it again.
func (c *Cache) setNativeInternal(key string, value any, vt ValueType, byteSize int64, expiration int64, suppressMutate bool) {
	newSize := int64(entryOverhead) + int64(len(key)) + byteSize
	oldSize := c.keySize(key)
	c.usedBytes += newSize - oldSize

	if prevPtr, had := c.items[key]; had && !prevPtr.IsNil() {
		c.lruRemove(prevPtr)
		delete(c.keysBySlot, prevPtr)
		delete(c.nativeValues, prevPtr)
		c.slabs.Free(prevPtr)
	}
	ptr := c.slabs.Alloc(0) // minimum class; data region unused for native
	c.keysBySlot[ptr] = key
	c.lruPushFront(ptr)

	meta := c.slabs.Meta(ptr)
	meta.ValueType = uint8(vt)
	meta.Encoding = uint8(EncNative)
	meta.NativeSize = uint32(byteSize)
	if expiration > 0 {
		meta.ExpirationNs = expiration
	} else {
		meta.ExpirationNs = 0
	}

	c.nativeValues[ptr] = value
	c.items[key] = ptr
	if !suppressMutate && c.OnMutate != nil {
		c.OnMutate(key)
	}
}

// valueTypeOf maps a Go-native value to its ObjType.
func valueTypeOf(value any) ValueType {
	switch value.(type) {
	case []string:
		return ObjTypeList
	case map[string]string:
		return ObjTypeHash
	case map[string]struct{}:
		return ObjTypeSet
	case *SortedSet:
		return ObjTypeSortedSet
	}
	return ObjTypeBytes
}

// nativePayloadSize returns just the value-shape byte cost (no key, no
// per-entry overhead). It walks the value once — used only when the
// caller does not provide an explicit size (snapshot loading, generic
// RawSet). On the hot path, handlers track size incrementally and call
// RawSetNativeWithSize instead.
func nativePayloadSize(key string, value any) int64 {
	full := estimateSize(key, value)
	return full - int64(entryOverhead) - int64(len(key))
}

// evictSampleSize is how many tail-end entries to sample per eviction
// pass. Mirrors Redis's maxmemory-samples default. Larger = closer to
// strict LRU but more CPU per eviction; smaller = approximate LRU but
// faster. 8 is the published Redis sweet spot and preserves the
// test-pinned semantic that "a key read recently survives eviction".
const evictSampleSize = 8

// evictLRU removes the entry with the oldest LastAccessNs from the tail
// region of the LRU list, repeating until delta bytes can be accommodated.
//
// Redis-style sampled approximate LRU: the linked list orders entries by
// last WRITE position (writes call lruPushFront / lruMoveToFront under
// the cache write lock). Reads via RawGet update LastAccessNs atomically
// but do NOT touch the list — the list mutation is unsafe under RLock.
// Eviction therefore samples up to evictSampleSize entries from the tail
// end of the list (oldest writes) and picks the one with the smallest
// LastAccessNs (oldest read). This preserves LRU correctness against
// read-heavy workloads even though the list itself is write-ordered.
func (c *Cache) evictLRU(ctx context.Context, delta int64) {
	for c.maxBytes > 0 && c.usedBytes+delta > c.maxBytes {
		if c.lruTail.IsNil() {
			break
		}
		victim := c.lruTail
		victimAccess := atomic.LoadInt64(&c.slabs.Meta(victim).LastAccessNs)
		node := c.slabs.Meta(victim).LRUPrev
		for i := 1; i < evictSampleSize && !node.IsNil(); i++ {
			access := atomic.LoadInt64(&c.slabs.Meta(node).LastAccessNs)
			if access < victimAccess {
				victim = node
				victimAccess = access
			}
			node = c.slabs.Meta(node).LRUPrev
		}
		evictKey, ok := c.keysBySlot[victim]
		if !ok {
			logger.Warn(ctx).Msg("evictLRU: keysBySlot has no entry for victim; bailing")
			break
		}
		logger.Debug(ctx).Str("key", evictKey).Msg("lru eviction")
		c.delete(evictKey)
	}
}

// RawGet returns the entry for key. The returned Entry is a value-type
// snapshot assembled from the slab slot's metadata and the native-value
// sidecar (if applicable).
//
// RawGet is safe under either the cache write lock OR the cache read lock.
// It does NOT move the entry in the LRU linked list (that requires
// exclusive access to the list head and neighbour pointers, which RLock
// cannot provide). It does update LastAccessNs via atomic.StoreInt64 so
// OBJECT IDLETIME and timestamp-based diagnostics stay accurate even on
// the read-lock-bypass path.
//
// Eviction uses lruTail (write order). Read-heavy access patterns no
// longer refresh the entry's LRU position; under memory pressure with
// long-lived hot reads the eviction may pick an entry that is still
// being read. This is the documented trade-off for the read-lock-bypass
// throughput gain — see projects/gocache/plans/command-flow/read-lock-bypass.
func (c *Cache) RawGet(key string) (Entry, bool) {
	ptr, found := c.items[key]
	if !found {
		return Entry{}, false
	}
	atomic.StoreInt64(&c.slabs.Meta(ptr).LastAccessNs, time.Now().UnixNano())
	return c.entryFromSlot(ptr), true
}

func (c *Cache) RawDelete(key string) {
	c.delete(key)
}

// TTLInternal returns the remaining TTL and a ValueState for key.
//
// States:
//
//	ValuePresent   — key exists with a future expiration (ExpirationNs > 0)
//	ValueExpired   — key has a TTL that has already passed (caller should
//	                 lazyExpire to clean it up)
//	ValueNoExpire  — key exists but no TTL is set
//	ValueAbsent    — key does not exist in the cache
//
// Callers that need to distinguish "missing" from "no TTL" (TTL/PTTL) can
// rely on ValueAbsent vs ValueNoExpire directly. Must be called while
// holding the cache read lock.
func (c *Cache) TTLInternal(key string) (time.Duration, ValueState) {
	ptr, exists := c.items[key]
	if !exists {
		return 0, ValueAbsent
	}
	expiration := c.slabs.Meta(ptr).ExpirationNs
	if expiration == 0 {
		return 0, ValueNoExpire
	}
	expirationTime := time.Unix(0, expiration)
	if expirationTime.Before(time.Now()) {
		return 0, ValueExpired
	}
	return time.Until(expirationTime), ValuePresent
}

func (c *Cache) delete(key string) {
	ptr, ok := c.items[key]
	if !ok {
		return
	}
	if !ptr.IsNil() {
		c.usedBytes -= c.chargedSize(key, ptr)
		c.lruRemove(ptr)
		delete(c.keysBySlot, ptr)
		delete(c.nativeValues, ptr)
		c.slabs.Free(ptr)
	}
	delete(c.items, key)
	if c.OnMutate != nil {
		c.OnMutate(key)
	}
}

// keySize returns the currently-charged byte cost for key, or 0 if absent.
// Used by set paths to compute the delta against usedBytes without a
// sidecar map. For packed entries the charge is entryOverhead + len(key) +
// len(value); for native entries it's estimateSize(key, nativeValue).
func (c *Cache) keySize(key string) int64 {
	ptr, ok := c.items[key]
	if !ok || ptr.IsNil() {
		return 0
	}
	return c.chargedSize(key, ptr)
}

// chargedSize returns the byte cost to subtract from usedBytes when freeing
// the slot identified by ptr (which maps to key). Packed entries use the
// slab-held value length; native entries read the cached SlotMeta.NativeSize
// (set when the value was written) so we never walk the map/slice on the hot
// path. See setInternal for where NativeSize is populated.
func (c *Cache) chargedSize(key string, ptr slab.SlabPointer) int64 {
	meta := c.slabs.Meta(ptr)
	enc := Encoding(meta.Encoding)
	if enc == EncPacked {
		return int64(entryOverhead) + int64(len(key)) + int64(c.slabs.Size(ptr))
	}
	return int64(entryOverhead) + int64(len(key)) + int64(meta.NativeSize)
}

// Range iterates over all cache entries. The callback receives a value-type
// Entry snapshot — mutating it has no effect on the cache. Return false
// to stop iteration.
func (c *Cache) Range(fn func(key string, entry Entry, expiration int64) bool) {
	for key, ptr := range c.items {
		if !fn(key, c.entryFromSlot(ptr), c.slabs.Meta(ptr).ExpirationNs) {
			break
		}
	}
}

// RawTTL returns the raw expiration timestamp in nanoseconds for the given key.
// Returns 0 if the key has no TTL set or the key is absent.
func (c *Cache) RawTTL(key string) int64 {
	ptr, ok := c.items[key]
	if !ok {
		return 0
	}
	return c.slabs.Meta(ptr).ExpirationNs
}

func (c *Cache) Clear(ctx context.Context) {
	logger.Info(ctx).Int("items", len(c.items)).Msg("cache cleared")
	c.items = make(map[string]slab.SlabPointer)
	c.nativeValues = make(map[slab.SlabPointer]any)
	c.keysBySlot = make(map[slab.SlabPointer]string)
	c.lruHead = slab.NilPointer
	c.lruTail = slab.NilPointer
	c.usedBytes = 0
	// Drop the entire slab arena — faster than walking entries to Free().
	c.slabs = slab.NewAllocator()
	if c.OnMutateAll != nil {
		c.OnMutateAll()
	}
}

// estimateSize returns an approximate memory usage in bytes for a key-value pair.
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
