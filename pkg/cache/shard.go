package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"gocache/api/logger"
	"gocache/pkg/cache/slab"
)

// Shard owns one slice of the keyspace plus all the state that goes with
// it: the items map, the slab allocator, an LRU list, and a memory
// budget. Every per-key cache method (RawGet, RawSet, RawDelete, …)
// resolves down to a method on Shard; multi-key methods live on the
// outer Cache and coordinate across shards.
//
// Concurrency contract: a Shard's mutex is the only lock that protects
// its fields. Callers must acquire the appropriate lock before invoking
// any shard method whose name does not start with `Lock` / `RLock`.
// Today's caller is the Cache's per-shard engine goroutine, which holds
// the write lock for the duration of one handler.
type Shard struct {
	mu             sync.RWMutex
	items          map[string]slab.SlabPointer
	nativeValues   map[slab.SlabPointer]any // populated only for EncNative entries
	keysBySlot     map[slab.SlabPointer]string
	slabs          *slab.Allocator
	lruHead        slab.SlabPointer
	lruTail        slab.SlabPointer
	usedBytes      int64
	maxBytes       int64
	evictionPolicy EvictionPolicy

	// onMutate / onMutateAll are set by the Cache constructor and forward
	// to the cache's external WATCH callbacks. Per-shard so the engine
	// goroutine can fire them without crossing back into Cache state.
	onMutate    func(key string)
	onMutateAll func()
}

func newShard(maxBytes int64, policy EvictionPolicy) *Shard {
	return &Shard{
		items:          make(map[string]slab.SlabPointer),
		nativeValues:   make(map[slab.SlabPointer]any),
		keysBySlot:     make(map[slab.SlabPointer]string),
		slabs:          slab.NewAllocator(),
		maxBytes:       maxBytes,
		evictionPolicy: policy,
	}
}

func (s *Shard) Lock()    { s.mu.Lock() }
func (s *Shard) Unlock()  { s.mu.Unlock() }
func (s *Shard) RLock()   { s.mu.RLock() }
func (s *Shard) RUnlock() { s.mu.RUnlock() }

// entryFromSlot reconstructs an Entry value from a slab slot. Caller
// must ensure ptr is live (present in keysBySlot). The returned Entry
// carries a back-reference to this shard so Entry-bound Cache methods
// (ResolvePacked, LastAccess) can dereference into the right slab.
func (s *Shard) entryFromSlot(ptr slab.SlabPointer) Entry {
	meta := s.slabs.Meta(ptr)
	enc := Encoding(meta.Encoding)
	var value any
	if enc == EncNative {
		value = s.nativeValues[ptr]
	}
	return Entry{
		ValueType: ValueType(meta.ValueType),
		Encoding:  enc,
		Value:     value,
		Ptr:       ptr,
		shard:     s,
	}
}

// LastAccess returns the wall-clock time of the last access to entry e.
func (s *Shard) LastAccess(e Entry) time.Time {
	if e.Ptr.IsNil() {
		return time.Time{}
	}
	ns := atomic.LoadInt64(&s.slabs.Meta(e.Ptr).LastAccessNs)
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// ResolvePacked returns a zero-copy view of e's packed bytes.
func (s *Shard) ResolvePacked(e Entry) []byte {
	if e.Encoding != EncPacked || e.Ptr.IsNil() {
		return nil
	}
	return s.slabs.Read(e.Ptr)
}

// SetExpiration updates only the TTL for an existing key. Returns true
// if the key existed. Must be called under the write lock.
func (s *Shard) SetExpiration(key string, expiration int64) bool {
	ptr, ok := s.items[key]
	if !ok {
		return false
	}
	if expiration > 0 {
		s.slabs.Meta(ptr).ExpirationNs = expiration
	} else {
		s.slabs.Meta(ptr).ExpirationNs = 0
	}
	if s.onMutate != nil {
		s.onMutate(key)
	}
	return true
}

// rename moves src's entry to dst in place. Caller guarantees both keys
// hash to this shard. Multi-shard RENAME is handled by the outer Cache.
// Must be called under the write lock.
func (s *Shard) rename(src, dst string, newExpiration int64) bool {
	ptr, ok := s.items[src]
	if !ok {
		return false
	}
	if _, exists := s.items[dst]; exists {
		s.delete(dst)
	}
	s.usedBytes += int64(len(dst)) - int64(len(src))

	s.items[dst] = ptr
	if !ptr.IsNil() {
		s.keysBySlot[ptr] = dst
		if newExpiration > 0 {
			s.slabs.Meta(ptr).ExpirationNs = newExpiration
		} else {
			s.slabs.Meta(ptr).ExpirationNs = 0
		}
	}
	delete(s.items, src)

	if s.onMutate != nil {
		s.onMutate(src)
		s.onMutate(dst)
	}
	return true
}

// rawSet stores key with value and expiration, enforcing the memory limit.
// Must be called under the write lock. ctx carries the operation for log
// correlation.
func (s *Shard) rawSet(ctx context.Context, key string, value any, expiration int64) error {
	if s.maxBytes > 0 {
		newSize := estimateSize(key, value)
		oldSize := s.keySize(key)
		delta := newSize - oldSize
		if delta > 0 && s.usedBytes+delta > s.maxBytes {
			switch s.evictionPolicy {
			case EvictionLRU:
				s.evictLRU(ctx, delta)
			case EvictionNone:
				logger.Warn(ctx).Str("key", key).Int64("usedBytes", s.usedBytes).Int64("maxBytes", s.maxBytes).Msg("write rejected, out of memory")
				return ErrOutOfMemory
			}
		}
	}
	s.setInternal(key, value, expiration, false)
	return nil
}

// rawLoad bypasses the memory limit check; intended for snapshot loading.
// Must be called under the write lock.
func (s *Shard) rawLoad(key string, value any, expiration int64) {
	s.setInternal(key, value, expiration, true)
}

// rawSetNativeWithSize stores a Go-native collection value at key using
// a caller-supplied byteSize so we don't walk the value to compute it.
// Must be called under the write lock.
func (s *Shard) rawSetNativeWithSize(ctx context.Context, key string, value any, byteSize int64, expiration int64) error {
	if s.maxBytes > 0 {
		newSize := int64(entryOverhead) + int64(len(key)) + byteSize
		oldSize := s.keySize(key)
		delta := newSize - oldSize
		if delta > 0 && s.usedBytes+delta > s.maxBytes {
			switch s.evictionPolicy {
			case EvictionLRU:
				s.evictLRU(ctx, delta)
			case EvictionNone:
				logger.Warn(ctx).Str("key", key).Int64("usedBytes", s.usedBytes).Int64("maxBytes", s.maxBytes).Msg("write rejected, out of memory")
				return ErrOutOfMemory
			}
		}
	}
	s.setNativeInternal(key, value, valueTypeOf(value), byteSize, expiration, false)
	return nil
}

// rawSetPacked stores a packed byte-encoded value for the given ValueType.
// Must be called under the write lock.
func (s *Shard) rawSetPacked(ctx context.Context, key string, vt ValueType, buf []byte, expiration int64) error {
	if s.maxBytes > 0 {
		newSize := estimateBytesSize(key, buf)
		oldSize := s.keySize(key)
		delta := newSize - oldSize
		if delta > 0 && s.usedBytes+delta > s.maxBytes {
			switch s.evictionPolicy {
			case EvictionLRU:
				s.evictLRU(ctx, delta)
			case EvictionNone:
				logger.Warn(ctx).Str("key", key).Int64("usedBytes", s.usedBytes).Int64("maxBytes", s.maxBytes).Msg("write rejected, out of memory")
				return ErrOutOfMemory
			}
		}
	}
	s.setPackedInternal(key, vt, buf, expiration, false)
	return nil
}

// rawLoadPacked stores a packed byte buffer, bypassing the memory limit
// check. Snapshot-loading uses this for entries with Encoding == EncPacked.
func (s *Shard) rawLoadPacked(key string, vt ValueType, buf []byte, expiration int64) {
	s.setPackedInternal(key, vt, buf, expiration, true)
}

// nativeSize returns the cached byte-size of an EncNative entry.
// Must be called while holding the read or write lock.
func (s *Shard) nativeSize(key string) int64 {
	ptr, ok := s.items[key]
	if !ok || ptr.IsNil() {
		return 0
	}
	return int64(s.slabs.Meta(ptr).NativeSize)
}

// rawGet returns the entry for key. Safe under either RLock or Lock; it
// updates LastAccessNs atomically without moving the entry in the LRU.
func (s *Shard) rawGet(key string) (Entry, bool) {
	ptr, found := s.items[key]
	if !found {
		return Entry{}, false
	}
	atomic.StoreInt64(&s.slabs.Meta(ptr).LastAccessNs, time.Now().UnixNano())
	return s.entryFromSlot(ptr), true
}

// rawDelete removes key. Must be called under the write lock.
func (s *Shard) rawDelete(key string) {
	s.delete(key)
}

// ttlInternal returns the remaining TTL and a ValueState for key.
func (s *Shard) ttlInternal(key string) (time.Duration, ValueState) {
	ptr, exists := s.items[key]
	if !exists {
		return 0, ValueAbsent
	}
	expiration := s.slabs.Meta(ptr).ExpirationNs
	if expiration == 0 {
		return 0, ValueNoExpire
	}
	expirationTime := time.Unix(0, expiration)
	if expirationTime.Before(time.Now()) {
		return 0, ValueExpired
	}
	return time.Until(expirationTime), ValuePresent
}

// rawTTL returns the raw expiration timestamp in nanoseconds.
func (s *Shard) rawTTL(key string) int64 {
	ptr, ok := s.items[key]
	if !ok {
		return 0
	}
	return s.slabs.Meta(ptr).ExpirationNs
}

// rangeShard iterates over this shard's entries. Must be called while
// holding the read or write lock.
func (s *Shard) rangeShard(fn func(key string, entry Entry, expiration int64) bool) bool {
	for key, ptr := range s.items {
		if !fn(key, s.entryFromSlot(ptr), s.slabs.Meta(ptr).ExpirationNs) {
			return false
		}
	}
	return true
}

// clear drops every entry on this shard. Must be called under the write lock.
func (s *Shard) clear() {
	s.items = make(map[string]slab.SlabPointer)
	s.nativeValues = make(map[slab.SlabPointer]any)
	s.keysBySlot = make(map[slab.SlabPointer]string)
	s.lruHead = slab.NilPointer
	s.lruTail = slab.NilPointer
	s.usedBytes = 0
	s.slabs = slab.NewAllocator()
}

// LRU helpers (per-shard) ---------------------------------------------------

func (s *Shard) lruPushFront(ptr slab.SlabPointer) {
	meta := s.slabs.Meta(ptr)
	meta.LRUPrev = slab.NilPointer
	meta.LRUNext = s.lruHead
	if !s.lruHead.IsNil() {
		s.slabs.Meta(s.lruHead).LRUPrev = ptr
	}
	s.lruHead = ptr
	if s.lruTail.IsNil() {
		s.lruTail = ptr
	}
	atomic.StoreInt64(&meta.LastAccessNs, time.Now().UnixNano())
}

func (s *Shard) lruRemove(ptr slab.SlabPointer) {
	meta := s.slabs.Meta(ptr)
	prev := meta.LRUPrev
	next := meta.LRUNext
	if !prev.IsNil() {
		s.slabs.Meta(prev).LRUNext = next
	} else {
		s.lruHead = next
	}
	if !next.IsNil() {
		s.slabs.Meta(next).LRUPrev = prev
	} else {
		s.lruTail = prev
	}
	meta.LRUPrev = slab.NilPointer
	meta.LRUNext = slab.NilPointer
}

func (s *Shard) lruMoveToFront(ptr slab.SlabPointer) {
	if s.lruHead == ptr {
		atomic.StoreInt64(&s.slabs.Meta(ptr).LastAccessNs, time.Now().UnixNano())
		return
	}
	s.lruRemove(ptr)
	s.lruPushFront(ptr)
}

func (s *Shard) setPackedInternal(key string, vt ValueType, buf []byte, expiration int64, suppressMutate bool) {
	newSize := estimateBytesSize(key, buf)
	oldSize := s.keySize(key)
	s.usedBytes += newSize - oldSize

	prevPtr, had := s.items[key]
	var ptr slab.SlabPointer

	switch {
	case had && !prevPtr.IsNil() && s.slabs.Capacity(prevPtr) >= uint32(len(buf)) &&
		Encoding(s.slabs.Meta(prevPtr).Encoding) == EncPacked:
		ptr = prevPtr
		s.slabs.Write(ptr, buf)
		s.lruMoveToFront(ptr)
	default:
		if had && !prevPtr.IsNil() {
			s.lruRemove(prevPtr)
			delete(s.keysBySlot, prevPtr)
			delete(s.nativeValues, prevPtr)
			s.slabs.Free(prevPtr)
		}
		ptr = s.slabs.Alloc(uint32(len(buf)))
		s.slabs.Write(ptr, buf)
		s.keysBySlot[ptr] = key
		s.lruPushFront(ptr)
	}

	meta := s.slabs.Meta(ptr)
	meta.ValueType = uint8(vt)
	meta.Encoding = uint8(EncPacked)
	if expiration > 0 {
		meta.ExpirationNs = expiration
	} else {
		meta.ExpirationNs = 0
	}

	s.items[key] = ptr
	if !suppressMutate && s.onMutate != nil {
		s.onMutate(key)
	}
}

func (s *Shard) setInternal(key string, value any, expiration int64, suppressMutate bool) {
	switch v := value.(type) {
	case []byte:
		s.setPackedInternal(key, ObjTypeBytes, v, expiration, suppressMutate)
		return
	case string:
		s.setPackedInternal(key, ObjTypeBytes, []byte(v), expiration, suppressMutate)
		return
	}
	s.setNativeInternal(key, value, valueTypeOf(value), nativePayloadSize(key, value), expiration, suppressMutate)
}

func (s *Shard) setNativeInternal(key string, value any, vt ValueType, byteSize int64, expiration int64, suppressMutate bool) {
	newSize := int64(entryOverhead) + int64(len(key)) + byteSize
	oldSize := s.keySize(key)
	s.usedBytes += newSize - oldSize

	if prevPtr, had := s.items[key]; had && !prevPtr.IsNil() {
		s.lruRemove(prevPtr)
		delete(s.keysBySlot, prevPtr)
		delete(s.nativeValues, prevPtr)
		s.slabs.Free(prevPtr)
	}
	ptr := s.slabs.Alloc(0)
	s.keysBySlot[ptr] = key
	s.lruPushFront(ptr)

	meta := s.slabs.Meta(ptr)
	meta.ValueType = uint8(vt)
	meta.Encoding = uint8(EncNative)
	meta.NativeSize = uint32(byteSize)
	if expiration > 0 {
		meta.ExpirationNs = expiration
	} else {
		meta.ExpirationNs = 0
	}

	s.nativeValues[ptr] = value
	s.items[key] = ptr
	if !suppressMutate && s.onMutate != nil {
		s.onMutate(key)
	}
}

// evictLRU evicts the entry with the oldest LastAccessNs (sampled from the
// tail of the LRU list) until delta bytes can be accommodated.
func (s *Shard) evictLRU(ctx context.Context, delta int64) {
	for s.maxBytes > 0 && s.usedBytes+delta > s.maxBytes {
		if s.lruTail.IsNil() {
			break
		}
		victim := s.lruTail
		victimAccess := atomic.LoadInt64(&s.slabs.Meta(victim).LastAccessNs)
		node := s.slabs.Meta(victim).LRUPrev
		for i := 1; i < evictSampleSize && !node.IsNil(); i++ {
			access := atomic.LoadInt64(&s.slabs.Meta(node).LastAccessNs)
			if access < victimAccess {
				victim = node
				victimAccess = access
			}
			node = s.slabs.Meta(node).LRUPrev
		}
		evictKey, ok := s.keysBySlot[victim]
		if !ok {
			logger.Warn(ctx).Msg("evictLRU: keysBySlot has no entry for victim; bailing")
			break
		}
		logger.Debug(ctx).Str("key", evictKey).Msg("lru eviction")
		s.delete(evictKey)
	}
}

func (s *Shard) delete(key string) {
	ptr, ok := s.items[key]
	if !ok {
		return
	}
	if !ptr.IsNil() {
		s.usedBytes -= s.chargedSize(key, ptr)
		s.lruRemove(ptr)
		delete(s.keysBySlot, ptr)
		delete(s.nativeValues, ptr)
		s.slabs.Free(ptr)
	}
	delete(s.items, key)
	if s.onMutate != nil {
		s.onMutate(key)
	}
}

func (s *Shard) keySize(key string) int64 {
	ptr, ok := s.items[key]
	if !ok || ptr.IsNil() {
		return 0
	}
	return s.chargedSize(key, ptr)
}

func (s *Shard) chargedSize(key string, ptr slab.SlabPointer) int64 {
	meta := s.slabs.Meta(ptr)
	enc := Encoding(meta.Encoding)
	if enc == EncPacked {
		return int64(entryOverhead) + int64(len(key)) + int64(s.slabs.Size(ptr))
	}
	return int64(entryOverhead) + int64(len(key)) + int64(meta.NativeSize)
}

func (s *Shard) slabStats() slab.Stats { return s.slabs.Stats() }
