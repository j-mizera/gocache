package observability

import (
	"sync"
	"sync/atomic"

	apiobs "gocache/api/observability"
)

// ShardedOperationTrackerManager is the singleton common
// OperationTrackerManager implementation. It owns a fixed set of pre-created
// tracker shards and routes each internal operation identity to exactly one
// shard tracker. The manager stores telemetry requests/state for later FIFO
// sidecar reaping; it does not materialize logs/events, call zerolog, build GCPC
// payloads, or fan out to plugins.
type ShardedOperationTrackerManager struct {
	trackers []apiobs.OperationTracker
	drainers []telemetryRecorder
	contexts connectionContextStore
}

// NewShardedOperationTrackerManager returns a singleton manager configured with
// shardCount pre-created tracker shards. Get is then only shard-index math plus
// array lookup, with no lazy creation mutex on the submit path.
func NewShardedOperationTrackerManager(shardCount, ringCapacity int) *ShardedOperationTrackerManager {
	if shardCount < 1 {
		shardCount = 1
	}
	manager := &ShardedOperationTrackerManager{
		trackers: make([]apiobs.OperationTracker, shardCount),
		drainers: make([]telemetryRecorder, shardCount),
	}
	for i := range manager.trackers {
		recorder := newMultiProducerRecorder(ringCapacity)
		manager.trackers[i] = newOperationTracker(recorder)
		manager.drainers[i] = recorder
	}
	manager.contexts.init()
	return manager
}

func (m *ShardedOperationTrackerManager) Get(operation apiobs.InternalOperationIdentity) apiobs.OperationTracker {
	return m.trackers[shardIndex(operation, len(m.trackers))]
}

func (m *ShardedOperationTrackerManager) UpdateConnectionContext(connection apiobs.ConnectionIdentity, pairs ...[]byte) apiobs.ConnectionContextVersion {
	return m.contexts.update(connection, pairs)
}

func (m *ShardedOperationTrackerManager) UpdateConnectionContextStrings(connection apiobs.ConnectionIdentity, pairs ...string) apiobs.ConnectionContextVersion {
	return m.contexts.updateStrings(connection, pairs)
}

func (m *ShardedOperationTrackerManager) RemoveConnectionContext(connection apiobs.ConnectionIdentity, keys ...[]byte) apiobs.ConnectionContextVersion {
	return m.contexts.remove(connection, keys)
}

func (m *ShardedOperationTrackerManager) RemoveConnectionContextStrings(connection apiobs.ConnectionIdentity, keys ...string) apiobs.ConnectionContextVersion {
	return m.contexts.removeStrings(connection, keys)
}

func (m *ShardedOperationTrackerManager) PinCurrentConnectionContextVersion(connection apiobs.ConnectionIdentity) apiobs.ConnectionContextVersion {
	return m.contexts.pinCurrent(connection)
}

func (m *ShardedOperationTrackerManager) RetainConnectionContextVersion(version apiobs.ConnectionContextVersion) bool {
	return m.contexts.retain(version)
}

func (m *ShardedOperationTrackerManager) ReleaseConnectionContextVersion(version apiobs.ConnectionContextVersion) bool {
	return m.contexts.release(version)
}

func (m *ShardedOperationTrackerManager) VisitConnectionContextVersion(version apiobs.ConnectionContextVersion, visitor apiobs.ConnectionContextVisitor) bool {
	return m.contexts.visit(version, visitor)
}

func (m *ShardedOperationTrackerManager) ForgetConnectionContext(connection apiobs.ConnectionIdentity) bool {
	return m.contexts.forget(connection)
}

func (m *ShardedOperationTrackerManager) ShardCount() int {
	return len(m.trackers)
}

func (m *ShardedOperationTrackerManager) DroppedRecords() uint64 {
	var dropped uint64
	for _, tracker := range m.trackers {
		dropped += tracker.DroppedRecords()
	}
	return dropped
}

// DrainShard drains accepted records from a pre-created tracker shard in FIFO
// order. It is the common worker surface used by tests and later server-owned
// reap callbacks that fold context mutations before subsequent log/event
// materialization; it is not part of the API contract exposed to plugins.
func (m *ShardedOperationTrackerManager) DrainShard(index int, fn func(apiobs.TelemetryRecord)) int {
	if index < 0 || index >= len(m.drainers) {
		return 0
	}
	return m.drainers[index].drain(fn)
}

type connectionContextStore struct {
	mu       sync.Mutex
	next     uint64
	current  atomic.Pointer[map[apiobs.ConnectionIdentity]apiobs.ConnectionContextVersion]
	registry atomic.Pointer[map[uint64]*ctxVersionEntry]
}

type ctxVersionEntry struct {
	id      uint64
	refs    atomic.Int64
	current atomic.Bool
	pairs   map[string]string
}

// retiredContextRefs marks a non-current, unreferenced version as logically
// reclaimed before a writer physically removes it from the RCU registry map.
const retiredContextRefs int64 = -1

func (s *connectionContextStore) init() {
	current := make(map[apiobs.ConnectionIdentity]apiobs.ConnectionContextVersion)
	registry := make(map[uint64]*ctxVersionEntry)
	s.current.Store(&current)
	s.registry.Store(&registry)
}

func (s *connectionContextStore) update(connection apiobs.ConnectionIdentity, pairs [][]byte) apiobs.ConnectionContextVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	base := s.currentPairsLocked(connection)
	next := clonePairs(base, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		next[string(pairs[i])] = string(pairs[i+1])
	}
	return s.replaceCurrentLocked(connection, next)
}

func (s *connectionContextStore) updateStrings(connection apiobs.ConnectionIdentity, pairs []string) apiobs.ConnectionContextVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	base := s.currentPairsLocked(connection)
	next := clonePairs(base, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		next[pairs[i]] = pairs[i+1]
	}
	return s.replaceCurrentLocked(connection, next)
}

func (s *connectionContextStore) remove(connection apiobs.ConnectionIdentity, keys [][]byte) apiobs.ConnectionContextVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	base := s.currentPairsLocked(connection)
	if base == nil {
		return 0
	}
	next := clonePairs(base, 0)
	for _, key := range keys {
		delete(next, string(key))
	}
	return s.replaceCurrentLocked(connection, next)
}

func (s *connectionContextStore) removeStrings(connection apiobs.ConnectionIdentity, keys []string) apiobs.ConnectionContextVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	base := s.currentPairsLocked(connection)
	if base == nil {
		return 0
	}
	next := clonePairs(base, 0)
	for _, key := range keys {
		delete(next, key)
	}
	return s.replaceCurrentLocked(connection, next)
}

func (s *connectionContextStore) pinCurrent(connection apiobs.ConnectionIdentity) apiobs.ConnectionContextVersion {
	current := s.loadCurrent()
	version := current[connection]
	if version == 0 {
		return 0
	}
	if !s.retain(version) {
		return 0
	}
	return version
}

func (s *connectionContextStore) retain(version apiobs.ConnectionContextVersion) bool {
	if version == 0 {
		return true
	}
	entry, ok := s.registryEntry(version)
	if !ok {
		return false
	}
	return retainEntry(entry)
}

func (s *connectionContextStore) release(version apiobs.ConnectionContextVersion) bool {
	if version == 0 {
		return true
	}
	entry, ok := s.registryEntry(version)
	if !ok {
		return false
	}
	return releaseEntry(entry)
}

func (s *connectionContextStore) visit(version apiobs.ConnectionContextVersion, visitor apiobs.ConnectionContextVisitor) bool {
	if version == 0 {
		return true
	}
	entry, ok := s.registryEntry(version)
	if !ok {
		return false
	}
	if entry.refs.Load() == retiredContextRefs {
		return false
	}
	if visitor == nil {
		return true
	}
	for key, value := range entry.pairs {
		if !visitor(key, value) {
			return true
		}
	}
	return true
}

func (s *connectionContextStore) forget(connection apiobs.ConnectionIdentity) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	current := s.loadCurrent()
	version := current[connection]
	if version == 0 {
		return false
	}
	registry := s.loadRegistry()
	entry, ok := registry[uint64(version)]
	nextCurrent := cloneCurrent(current, 0)
	delete(nextCurrent, connection)
	s.current.Store(&nextCurrent)
	if ok {
		entry.current.Store(false)
		retireEntryIfUnreferenced(entry)
	}
	if ok && entry.refs.Load() == retiredContextRefs {
		nextRegistry := cloneRegistry(registry, 0, func(id uint64, candidate *ctxVersionEntry) bool {
			return id == uint64(version) && candidate != nil && candidate.refs.Load() == retiredContextRefs
		})
		s.registry.Store(&nextRegistry)
	}
	if !ok {
		return false
	}
	return true
}

func (s *connectionContextStore) reclaim() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	registry := s.loadRegistry()
	nextRegistry := cloneRegistry(registry, 0, func(_ uint64, entry *ctxVersionEntry) bool {
		return retireEntryIfUnreferenced(entry)
	})
	s.registry.Store(&nextRegistry)
}

func (s *connectionContextStore) ensureLocked() {
	if s.current.Load() == nil || s.registry.Load() == nil {
		s.init()
	}
}

func (s *connectionContextStore) currentPairsLocked(connection apiobs.ConnectionIdentity) map[string]string {
	current := s.loadCurrent()
	version := current[connection]
	if version == 0 {
		return nil
	}
	registry := s.loadRegistry()
	entry := registry[uint64(version)]
	if entry == nil || entry.refs.Load() == retiredContextRefs {
		return nil
	}
	return entry.pairs
}

func (s *connectionContextStore) replaceCurrentLocked(connection apiobs.ConnectionIdentity, pairs map[string]string) apiobs.ConnectionContextVersion {
	current := s.loadCurrent()
	registry := s.loadRegistry()
	old := current[connection]
	oldEntry := registry[uint64(old)]
	s.next++
	version := apiobs.ConnectionContextVersion(s.next)
	entry := &ctxVersionEntry{
		id:    uint64(version),
		pairs: pairs,
	}
	entry.current.Store(true)
	nextRegistry := cloneRegistry(registry, 1, nil)
	nextRegistry[uint64(version)] = entry
	nextCurrent := cloneCurrent(current, 1)
	nextCurrent[connection] = version
	s.registry.Store(&nextRegistry)
	s.current.Store(&nextCurrent)
	if oldEntry != nil {
		oldEntry.current.Store(false)
		retireEntryIfUnreferenced(oldEntry)
		if oldEntry.refs.Load() == retiredContextRefs {
			reclaimedRegistry := cloneRegistry(nextRegistry, 0, func(id uint64, candidate *ctxVersionEntry) bool {
				return id == uint64(old) && candidate != nil && candidate.refs.Load() == retiredContextRefs
			})
			s.registry.Store(&reclaimedRegistry)
		}
	}
	return version
}

func (s *connectionContextStore) loadCurrent() map[apiobs.ConnectionIdentity]apiobs.ConnectionContextVersion {
	current := s.current.Load()
	if current == nil {
		return nil
	}
	return *current
}

func (s *connectionContextStore) loadRegistry() map[uint64]*ctxVersionEntry {
	registry := s.registry.Load()
	if registry == nil {
		return nil
	}
	return *registry
}

func (s *connectionContextStore) registryEntry(version apiobs.ConnectionContextVersion) (*ctxVersionEntry, bool) {
	registry := s.loadRegistry()
	entry, ok := registry[uint64(version)]
	if !ok || entry == nil {
		return nil, false
	}
	return entry, true
}

func retainEntry(entry *ctxVersionEntry) bool {
	for {
		refs := entry.refs.Load()
		if refs == retiredContextRefs {
			return false
		}
		if entry.refs.CompareAndSwap(refs, refs+1) {
			return true
		}
	}
}

func releaseEntry(entry *ctxVersionEntry) bool {
	for {
		refs := entry.refs.Load()
		if refs == retiredContextRefs {
			return false
		}
		if refs == 0 {
			retireEntryIfUnreferenced(entry)
			return true
		}
		if entry.refs.CompareAndSwap(refs, refs-1) {
			if refs == 1 {
				retireEntryIfUnreferenced(entry)
			}
			return true
		}
	}
}

func retireEntryIfUnreferenced(entry *ctxVersionEntry) bool {
	if entry == nil {
		return false
	}
	for {
		refs := entry.refs.Load()
		if refs == retiredContextRefs {
			return true
		}
		if refs != 0 || entry.current.Load() {
			return false
		}
		if entry.refs.CompareAndSwap(0, retiredContextRefs) {
			return true
		}
	}
}

func cloneCurrent(base map[apiobs.ConnectionIdentity]apiobs.ConnectionContextVersion, extra int) map[apiobs.ConnectionIdentity]apiobs.ConnectionContextVersion {
	if extra < 0 {
		extra = 0
	}
	clone := make(map[apiobs.ConnectionIdentity]apiobs.ConnectionContextVersion, len(base)+extra)
	for connection, version := range base {
		clone[connection] = version
	}
	return clone
}

func cloneRegistry(base map[uint64]*ctxVersionEntry, extra int, omit func(uint64, *ctxVersionEntry) bool) map[uint64]*ctxVersionEntry {
	if extra < 0 {
		extra = 0
	}
	clone := make(map[uint64]*ctxVersionEntry, len(base)+extra)
	for version, entry := range base {
		if omit != nil && omit(version, entry) {
			continue
		}
		clone[version] = entry
	}
	return clone
}

func clonePairs(base map[string]string, extra int) map[string]string {
	if extra < 0 {
		extra = 0
	}
	clone := make(map[string]string, len(base)+extra)
	for key, value := range base {
		clone[key] = value
	}
	return clone
}
