package observability

import (
	"sync"

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
	current  map[apiobs.ConnectionIdentity]apiobs.ConnectionContextVersion
	versions map[apiobs.ConnectionContextVersion]*connectionContextVersion
}

type connectionContextVersion struct {
	connection apiobs.ConnectionIdentity
	current    bool
	refs       uint64
	pairs      map[string]string
}

func (s *connectionContextStore) init() {
	s.current = make(map[apiobs.ConnectionIdentity]apiobs.ConnectionContextVersion)
	s.versions = make(map[apiobs.ConnectionContextVersion]*connectionContextVersion)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	version := s.current[connection]
	if version == 0 {
		return 0
	}
	s.versions[version].refs++
	return version
}

func (s *connectionContextStore) retain(version apiobs.ConnectionContextVersion) bool {
	if version == 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	entry, ok := s.versions[version]
	if !ok {
		return false
	}
	entry.refs++
	return true
}

func (s *connectionContextStore) release(version apiobs.ConnectionContextVersion) bool {
	if version == 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	entry, ok := s.versions[version]
	if !ok {
		return false
	}
	if entry.refs > 0 {
		entry.refs--
	}
	if entry.refs == 0 && !entry.current {
		delete(s.versions, version)
	}
	return true
}

func (s *connectionContextStore) visit(version apiobs.ConnectionContextVersion, visitor apiobs.ConnectionContextVisitor) bool {
	if version == 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	entry, ok := s.versions[version]
	if !ok {
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
	version := s.current[connection]
	if version == 0 {
		return false
	}
	delete(s.current, connection)
	entry, ok := s.versions[version]
	if !ok {
		return false
	}
	entry.current = false
	if entry.refs == 0 {
		delete(s.versions, version)
	}
	return true
}

func (s *connectionContextStore) reclaim() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLocked()
	for version, entry := range s.versions {
		if entry.refs == 0 && !entry.current {
			delete(s.versions, version)
		}
	}
}

func (s *connectionContextStore) ensureLocked() {
	if s.current == nil || s.versions == nil {
		s.init()
	}
}

func (s *connectionContextStore) currentPairsLocked(connection apiobs.ConnectionIdentity) map[string]string {
	version := s.current[connection]
	if version == 0 {
		return nil
	}
	return s.versions[version].pairs
}

func (s *connectionContextStore) replaceCurrentLocked(connection apiobs.ConnectionIdentity, pairs map[string]string) apiobs.ConnectionContextVersion {
	old := s.current[connection]
	if old != 0 {
		entry := s.versions[old]
		entry.current = false
		if entry.refs == 0 {
			delete(s.versions, old)
		}
	}
	s.next++
	version := apiobs.ConnectionContextVersion(s.next)
	s.current[connection] = version
	s.versions[version] = &connectionContextVersion{
		connection: connection,
		current:    true,
		pairs:      pairs,
	}
	return version
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
