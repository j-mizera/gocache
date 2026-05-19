package cache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gammazero/deque"

	apiconfig "gocache/api/config"
	"gocache/api/logger"
	"gocache/pkg/cache/slab"
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

// Collection-element overhead constants. Single source of truth for both
// estimateSize (full walk) and handler-side incremental tracking.
const (
	HashFieldOverhead   = 32 // per field: key + value pointer + map bucket
	ListElementOverhead = 16 // per element: slice header amortisation
	SetMemberOverhead   = 16 // per member: map bucket amortisation
	ZSetMemberOverhead  = 24 // per member: float64 score + map bucket
)

// bytesPerMB converts a megabyte limit to bytes for the cache's byte budget.
const bytesPerMB int64 = 1024 * 1024

// evictSampleSize is how many tail-end entries to sample per eviction
// pass. Mirrors Redis's maxmemory-samples default. Larger = closer to
// strict LRU but more CPU per eviction; smaller = approximate LRU but
// faster. 8 is the published Redis sweet spot and preserves the
// test-pinned semantic that "a key read recently survives eviction".
const evictSampleSize = 8

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

// Entry is a value-type snapshot of a cache entry. It is assembled on
// demand by RawGet / Range from a shard's slab meta and the native-value
// sidecar; callers never store *Entry in the cache. The unexported
// shard back-reference is set by Shard.entryFromSlot so Entry-bound
// Cache methods (ResolvePacked, LastAccess) can dereference into the
// right shard's slab without a separate routing table.
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

	// shard back-references the shard that produced this Entry. Set by
	// Shard.entryFromSlot. Nil for the zero value (used as "no entry"
	// sentinel by code that holds an Entry beyond its shard's lock).
	shard *Shard
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

// Cache is a thin router over one or more Shards. Today the production
// configuration uses N=1 (single shard, identical behaviour to the
// pre-refactor cache); a follow-up bumps N to spread per-key contention
// across multiple engine goroutines.
//
// Cache holds the configuration (PackedThresholds, eviction policy,
// memory budget, WATCH callbacks); the per-key state (items map, slab
// allocator, LRU list, used bytes) lives on each Shard.
type Cache struct {
	shards []*Shard
	n      uint64

	// Shared configuration / immutable-after-construction fields.
	configMu       sync.Mutex // guards the runtime-mutable config below
	maxBytes       int64      // 0 = unlimited; mirrored to each shard
	evictionPolicy EvictionPolicy
	packed         PackedThresholds

	// OnMutate / OnMutateAll are external callbacks (WATCH dirty-bit
	// propagation). Set after construction by the server wiring.
	OnMutate    func(key string)
	OnMutateAll func()
}

// DefaultShards aliases the config-layer default so cache constructors
// that don't take an explicit shard count stay in sync with the YAML
// config path. Must be a positive power of two.
const DefaultShards = apiconfig.DefaultCacheShards

// New constructs a Cache with the default shard count, no memory limit,
// and LRU eviction.
func New() *Cache {
	return newCache(DefaultShards, 0, EvictionLRU)
}

// NewWithConfig constructs a Cache with the default shard count from a
// megabyte limit. Use NewWithShards to override the shard count.
func NewWithConfig(maxMemoryMB int64, policy EvictionPolicy) *Cache {
	return NewWithShards(DefaultShards, maxMemoryMB, policy)
}

// NewWithShards constructs a Cache with a specific shard count and
// memory limit. shardCount must be a positive power of two; non-power-
// of-two values are rounded down to the nearest power of two (so the
// per-key fast path stays a single mask rather than a mod).
func NewWithShards(shardCount int, maxMemoryMB int64, policy EvictionPolicy) *Cache {
	var maxBytes int64
	if maxMemoryMB > 0 {
		maxBytes = maxMemoryMB * bytesPerMB
	}
	return newCache(roundDownPow2(shardCount), maxBytes, policy)
}

// NewWithBytes creates a single-shard cache with a raw byte limit.
// Intended for testing where the budget is tight (a few hundred bytes)
// — at the production default shard count (8) the per-shard slice of
// such a budget would round to zero and trip every write. Tests that
// want sharded byte-precise behaviour can call newCache directly.
func NewWithBytes(maxBytes int64, policy EvictionPolicy) *Cache {
	return newCache(1, maxBytes, policy)
}

// roundDownPow2 returns the largest power of two ≤ n, or 1 for n ≤ 1.
func roundDownPow2(n int) int {
	if n <= 1 {
		return 1
	}
	p := 1
	for p<<1 <= n {
		p <<= 1
	}
	return p
}

func newCache(shards int, maxBytes int64, policy EvictionPolicy) *Cache {
	if shards <= 0 {
		shards = 1
	}
	c := &Cache{
		n:              uint64(shards),
		shards:         make([]*Shard, shards),
		maxBytes:       maxBytes,
		evictionPolicy: policy,
		packed: PackedThresholds{
			HashMaxEntries: apiconfig.DefaultHashMaxPackedEntries,
			HashMaxValue:   apiconfig.DefaultHashMaxPackedValue,
			SetMaxEntries:  apiconfig.DefaultSetMaxPackedEntries,
			SetMaxValue:    apiconfig.DefaultSetMaxPackedValue,
			ZSetMaxEntries: apiconfig.DefaultZSetMaxPackedEntries,
			ZSetMaxValue:   apiconfig.DefaultZSetMaxPackedValue,
			ListMaxBytes:   apiconfig.DefaultListMaxPackedSize,
		},
	}
	// Each shard receives an equal slice of the memory budget.
	perShardBytes := maxBytes
	if shards > 1 && maxBytes > 0 {
		perShardBytes = maxBytes / int64(shards)
	}
	// Scale each shard's slab target so the total slab capacity across
	// all shards stays roughly constant rather than growing linearly with
	// N. At N=1 each shard gets the default 1 MiB; at N=16 each shard
	// gets 64 KiB. The slab package floors at MinTargetSlabBytes (64 KiB)
	// to keep the per-class entry count sensible.
	slabTarget := slab.DefaultTargetSlabBytes / uint32(shards)
	for i := range c.shards {
		c.shards[i] = newShard(perShardBytes, policy, slabTarget)
	}
	c.bindShardCallbacks()
	return c
}

// bindShardCallbacks wires each shard's onMutate to the Cache's external
// OnMutate. Called from constructors and after SetOnMutate.
func (c *Cache) bindShardCallbacks() {
	for _, s := range c.shards {
		s.onMutate = func(key string) {
			if c.OnMutate != nil {
				c.OnMutate(key)
			}
		}
		s.onMutateAll = func() {
			if c.OnMutateAll != nil {
				c.OnMutateAll()
			}
		}
	}
}

// Shard returns the shard owning key. Used by the engine to route a
// command to the right per-shard goroutine. Exported so callers outside
// the package (the engine, the evaluator) can compute the routing the
// same way.
func (c *Cache) Shard(key string) *Shard {
	return c.shards[c.shardIndex(key)]
}

// ShardByIndex returns the i-th shard. Bounds-checked.
func (c *Cache) ShardByIndex(i int) *Shard {
	if i < 0 || i >= len(c.shards) {
		return nil
	}
	return c.shards[i]
}

// ShardCount returns the total number of shards.
func (c *Cache) ShardCount() int { return len(c.shards) }

// shardIndex maps a key to its shard index. fnv1a64 over the key bytes
// masked by (n-1) — the constructors guarantee n is a power of two so
// this is a single AND instruction. At N=1 the mask is zero and every
// key trivially routes to shard 0.
func (c *Cache) shardIndex(key string) int {
	return int(fnv1a(key) & (c.n - 1))
}

// ShardIndexOf is the exported routing helper used by the evaluator
// before dispatching a single-key handler. Returns the shard index for
// key, computed the same way Shard(key) routes — callers that need the
// pointer should use Shard(key) instead.
func (c *Cache) ShardIndexOf(key string) int { return c.shardIndex(key) }

// fnv1a is the 64-bit FNV-1a hash. Allocation-free, no library dependency.
// At N=1 the caller short-circuits before reaching here. Replaceable with
// a faster hash (xxhash) in a follow-up; today FNV avoids adding a new
// direct dependency to pkg/cache for what is currently a no-op routing.
func fnv1a(s string) uint64 {
	const offset uint64 = 14695981039346656037
	const prime uint64 = 1099511628211
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// Single-key API ------------------------------------------------------------
//
// Each method routes to the shard owning key and forwards. Locking is
// the caller's responsibility — the engine goroutine for shard X holds
// shard X's lock for the duration of one handler.

func (c *Cache) RawGet(key string) (Entry, bool) { return c.Shard(key).rawGet(key) }
func (c *Cache) RawDelete(key string)            { c.Shard(key).rawDelete(key) }
func (c *Cache) RawTTL(key string) int64         { return c.Shard(key).rawTTL(key) }
func (c *Cache) NativeSize(key string) int64     { return c.Shard(key).nativeSize(key) }
func (c *Cache) TTLInternal(key string) (time.Duration, ValueState) {
	return c.Shard(key).ttlInternal(key)
}
func (c *Cache) SetExpiration(key string, expiration int64) bool {
	return c.Shard(key).SetExpiration(key, expiration)
}
func (c *Cache) RawSet(ctx context.Context, key string, value any, expiration int64) error {
	return c.Shard(key).rawSet(ctx, key, value, expiration)
}
func (c *Cache) RawLoad(key string, value any, expiration int64) {
	c.Shard(key).rawLoad(key, value, expiration)
}
func (c *Cache) RawSetNativeWithSize(ctx context.Context, key string, value any, byteSize int64, expiration int64) error {
	return c.Shard(key).rawSetNativeWithSize(ctx, key, value, byteSize, expiration)
}
func (c *Cache) RawSetPacked(ctx context.Context, key string, vt ValueType, buf []byte, expiration int64) error {
	return c.Shard(key).rawSetPacked(ctx, key, vt, buf, expiration)
}
func (c *Cache) RawLoadPacked(key string, vt ValueType, buf []byte, expiration int64) {
	c.Shard(key).rawLoadPacked(key, vt, buf, expiration)
}

// Entry-bound methods -------------------------------------------------------
//
// Entry carries a back-reference to its source shard, so these forward
// without re-routing.

func (c *Cache) LastAccess(e Entry) time.Time {
	if e.shard == nil {
		return time.Time{}
	}
	return e.shard.LastAccess(e)
}

func (c *Cache) ResolvePacked(e Entry) []byte {
	if e.shard == nil {
		return nil
	}
	return e.shard.ResolvePacked(e)
}

// Multi-key / cross-shard API ----------------------------------------------

// Rename moves src's entry to dst. Same-shard takes the slot-preserving
// fast path; cross-shard re-encodes through the regular write path on
// dst's shard. Caller must hold all shard locks (the multi-key dispatch
// path acquires them via Cache.Lock).
func (c *Cache) Rename(src, dst string, newExpiration int64) bool {
	srcShard := c.Shard(src)
	dstShard := c.Shard(dst)
	if srcShard == dstShard {
		return srcShard.rename(src, dst, newExpiration)
	}
	entry, found := srcShard.rawGet(src)
	if !found {
		return false
	}

	// Extract the value before deleting src so we can rewrite it on dst.
	// Packed bytes alias src's slab; copy because the slab slot is freed
	// by srcShard.delete.
	var packedValue []byte
	var nativeValue any
	if entry.Encoding == EncPacked {
		buf := srcShard.ResolvePacked(entry)
		packedValue = append([]byte(nil), buf...)
	} else {
		nativeValue = entry.Value
	}
	nativeBytes := srcShard.nativeSize(src)

	if _, dstFound := dstShard.rawGet(dst); dstFound {
		dstShard.delete(dst)
	}
	srcShard.delete(src)

	if entry.Encoding == EncPacked {
		dstShard.setPackedInternal(dst, entry.ValueType, packedValue, newExpiration, true)
	} else {
		dstShard.setNativeInternal(dst, nativeValue, entry.ValueType, nativeBytes, newExpiration, true)
	}
	if c.OnMutate != nil {
		c.OnMutate(src)
		c.OnMutate(dst)
	}
	return true
}

// Range iterates over every entry across every shard. Caller must
// already hold each shard's appropriate lock — today the engine path
// holds the (single) shard's write lock for the duration of one handler.
// At N>1 a multi-shard Range needs cross-shard locking; deferred.
func (c *Cache) Range(fn func(key string, entry Entry, expiration int64) bool) {
	for _, s := range c.shards {
		if !s.rangeShard(fn) {
			return
		}
	}
}

// Clear drops every entry on every shard. Caller holds the necessary
// locks (today: the single shard's write lock via the engine path).
func (c *Cache) Clear(ctx context.Context) {
	totalItems := c.lenLocked()
	logger.Info(ctx).Int("items", totalItems).Msg("cache cleared")
	for _, s := range c.shards {
		s.clear()
	}
	if c.OnMutateAll != nil {
		c.OnMutateAll()
	}
}

// Bulk locking --------------------------------------------------------------
//
// Lock / Unlock / RLock / RUnlock acquire every shard's mutex in shard-id
// order (release in reverse). Used by tools and tests that need a globally
// consistent view — bench harnesses, snapshot save/load, and the engine
// for legacy single-engine semantics. The hot path uses per-shard locks
// inside the per-shard engine goroutine and never reaches these.

func (c *Cache) Lock() {
	for _, s := range c.shards {
		s.Lock()
	}
}

func (c *Cache) Unlock() {
	for i := len(c.shards) - 1; i >= 0; i-- {
		c.shards[i].Unlock()
	}
}

func (c *Cache) RLock() {
	for _, s := range c.shards {
		s.RLock()
	}
}

func (c *Cache) RUnlock() {
	for i := len(c.shards) - 1; i >= 0; i-- {
		c.shards[i].RUnlock()
	}
}

// LockShards acquires the listed shards in ascending shard-id order;
// the returned closure releases them in reverse order. Used by
// multi-key handlers that touch a known subset of shards (MGET, MSET,
// RENAME, etc.) to avoid the full bulk-lock cost.
//
// shardIDs must be a sorted slice of unique indices in [0, ShardCount).
// Callers obtain a properly-shaped slice from TouchedShards. write
// selects RWMutex.Lock vs RLock on every listed shard.
//
// The sorted-acquisition discipline prevents deadlock between concurrent
// callers — any two LockShards calls acquire shared shards in the same
// order, so no cycle can form.
func (c *Cache) LockShards(shardIDs []int, write bool) func() {
	if len(shardIDs) == 0 {
		return func() {}
	}
	for _, id := range shardIDs {
		s := c.shards[id]
		if write {
			s.Lock()
		} else {
			s.RLock()
		}
	}
	if write {
		return func() {
			for i := len(shardIDs) - 1; i >= 0; i-- {
				c.shards[shardIDs[i]].Unlock()
			}
		}
	}
	return func() {
		for i := len(shardIDs) - 1; i >= 0; i-- {
			c.shards[shardIDs[i]].RUnlock()
		}
	}
}

// TouchedShards returns the sorted unique shard indices for the given
// keys. Allocation-light: uses a uint64 bitset for the dedup (works
// for ShardCount ≤ 64, which covers the practical range up to and
// well beyond the prototype's measured-optimum N=16) and allocates
// one small result slice. Callers pass the slice straight to
// LockShards or to command.Context.TouchedShards.
func (c *Cache) TouchedShards(keys []string) []int {
	if len(keys) == 0 {
		return nil
	}
	if c.n <= 64 {
		var mask uint64
		for _, k := range keys {
			mask |= 1 << uint(c.shardIndex(k))
		}
		out := make([]int, 0, popcount(mask))
		for i := 0; mask != 0; i++ {
			if mask&1 != 0 {
				out = append(out, i)
			}
			mask >>= 1
		}
		return out
	}
	// N > 64 fallback: dedup via map. Not on the hot path today.
	seen := make(map[int]struct{}, len(keys))
	out := make([]int, 0, len(keys))
	for _, k := range keys {
		s := c.shardIndex(k)
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	// Insert sort — small N, allocation-free.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func popcount(x uint64) int {
	x = x - ((x >> 1) & 0x5555555555555555)
	x = (x & 0x3333333333333333) + ((x >> 2) & 0x3333333333333333)
	x = (x + (x >> 4)) & 0x0f0f0f0f0f0f0f0f
	return int((x * 0x0101010101010101) >> 56)
}

// Aggregates and config -----------------------------------------------------

// Len returns the number of keys across every shard. Caller is
// responsible for any lock coherency they require — at N=1 the single
// shard's lock provides a consistent view.
func (c *Cache) Len() int { return c.lenLocked() }

func (c *Cache) lenLocked() int {
	n := 0
	for _, s := range c.shards {
		n += len(s.items)
	}
	return n
}

// UsedBytes sums each shard's accounting.
func (c *Cache) UsedBytes() int64 {
	var total int64
	for _, s := range c.shards {
		total += s.usedBytes
	}
	return total
}

// MaxBytes returns the configured memory limit in bytes (0 = unlimited).
// The cache's budget is split evenly across shards; this method returns
// the total, not the per-shard slice.
func (c *Cache) MaxBytes() int64 {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	return c.maxBytes
}

// SetMemoryLimit updates the memory limit and eviction policy at runtime.
// Each shard receives an equal slice of the new total.
func (c *Cache) SetMemoryLimit(ctx context.Context, maxMemoryMB int64, policy EvictionPolicy) {
	c.configMu.Lock()
	if maxMemoryMB > 0 {
		c.maxBytes = maxMemoryMB * bytesPerMB
	} else {
		c.maxBytes = 0
	}
	c.evictionPolicy = policy
	maxBytes := c.maxBytes
	c.configMu.Unlock()

	perShard := maxBytes
	if len(c.shards) > 1 && maxBytes > 0 {
		perShard = maxBytes / int64(len(c.shards))
	}
	for _, s := range c.shards {
		s.Lock()
		s.maxBytes = perShard
		s.evictionPolicy = policy
		// Evict if the new limit is below current usage on this shard.
		if s.maxBytes > 0 && s.usedBytes > s.maxBytes && s.evictionPolicy == EvictionLRU {
			s.evictLRU(ctx, 0)
		}
		s.Unlock()
	}
	logger.Info(ctx).Int64("maxBytes", maxBytes).Str("policy", c.EvictionPolicyString()).Msg("memory limit updated")
}

// EvictionPolicyString returns the eviction policy as a human-readable string.
func (c *Cache) EvictionPolicyString() string {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	switch c.evictionPolicy {
	case EvictionLRU:
		return "allkeys-lru"
	default:
		return "noeviction"
	}
}

// PackedThresholds returns the current hybrid-encoding thresholds.
func (c *Cache) PackedThresholds() PackedThresholds {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	return c.packed
}

// SetPackedThresholds replaces the hybrid-encoding thresholds.
func (c *Cache) SetPackedThresholds(t PackedThresholds) {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	c.packed = t
}

// SlabStats returns aggregated slab stats across every shard.
func (c *Cache) SlabStats() slab.Stats {
	var total slab.Stats
	for _, s := range c.shards {
		st := s.slabStats()
		total.TotalAllocs += st.TotalAllocs
		total.TotalFrees += st.TotalFrees
		total.LiveEntries += st.LiveEntries
		total.CapacityBytes += st.CapacityBytes
		total.AllocatedBytes += st.AllocatedBytes
		total.HugeCount += st.HugeCount
		total.HugeBytes += st.HugeBytes
		for i := range total.PerClassInUse {
			total.PerClassInUse[i] += st.PerClassInUse[i]
			total.PerClassSlabs[i] += st.PerClassSlabs[i]
		}
	}
	return total
}

// Helpers -------------------------------------------------------------------

// estimateBytesSize charges the exact encoded size of a packed []byte value
// plus the key and per-entry overhead.
func estimateBytesSize(key string, buf []byte) int64 {
	return int64(entryOverhead) + int64(len(key)) + int64(len(buf))
}

// valueTypeOf maps a Go-native value to its ObjType.
func valueTypeOf(value any) ValueType {
	switch value.(type) {
	case *deque.Deque[string]:
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

// estimateSize returns an approximate memory usage in bytes for a key-value pair.
func estimateSize(key string, value any) int64 {
	size := int64(entryOverhead) + int64(len(key))
	switch v := value.(type) {
	case []byte:
		size += int64(len(v))
	case string:
		size += int64(len(v))
	case *deque.Deque[string]:
		for i := 0; i < v.Len(); i++ {
			size += int64(len(v.At(i))) + ListElementOverhead
		}
	case map[string]string:
		for k, val := range v {
			size += int64(len(k)) + int64(len(val)) + HashFieldOverhead
		}
	case map[string]struct{}:
		for k := range v {
			size += int64(len(k)) + SetMemberOverhead
		}
	case *SortedSet:
		size += v.EstimateSize()
	}
	return size
}
