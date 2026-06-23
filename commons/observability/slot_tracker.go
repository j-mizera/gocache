package observability

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apicommand "gocache/api/command"
	apiobs "gocache/api/observability"
)

const defaultSlotMagazineCapacity = 16

const (
	defaultHotShardGrowthOccupancyThreshold = 0.85
	defaultHotShardGrowthShrinkThreshold    = 0.30
	defaultHotShardGrowthSustainedChecks    = 3
	defaultHotShardGrowthMaxSegments        = 2
	defaultHotShardGrowthCheckInterval      = 100 * time.Millisecond
)

// SlotTerminalStatus records why an operation slot became terminal.
type SlotTerminalStatus uint8

const (
	SlotTerminalUnknown SlotTerminalStatus = iota
	SlotTerminalFinished
	SlotTerminalFailed
	SlotTerminalTimedOut
	SlotTerminalAbandoned
)

// SlotTrackerConfig configures the preallocated operation-slot storage engine.
type SlotTrackerConfig struct {
	ShardCount              int
	MinSegmentsPerShard     int
	MaxSegmentsPerShard     int
	SegmentSize             int
	MagazineCapacity        int
	RecordsPerOperation     int
	CompletedRingPerShard   int
	ReleaseContextVersionFn func(apiobs.ConnectionContextVersion) bool
	HotShardGrowth          HotShardGrowthConfig
}

// HotShardGrowthConfig configures an optional off-path pressure monitor that
// grows or retires slot segments for sustained per-shard occupancy. The zero
// value is disabled and adds no goroutine.
type HotShardGrowthConfig struct {
	Enabled            bool
	OccupancyThreshold float64
	SustainedChecks    int
	MaxGrowthSegments  int
	ShrinkThreshold    float64
	CheckInterval      time.Duration
}

// InternalTrackerHandle is an implementation-only handle for shard-owned slot
// storage. It is intentionally separate from api/observability.OperationHandle.
type InternalTrackerHandle struct {
	shard      uint16
	segment    uint16
	slot       uint32
	generation uint32
	segmentRef *operationSegment
	slotRef    *operationSlot
}

// SlotMagazine is an owner-side cache for batched slot allocation.
// The zero value is ready for use by a single connection goroutine.
type SlotMagazine struct {
	refs []slotRef
}

func (m *SlotMagazine) pop() (slotRef, bool) {
	if m == nil || len(m.refs) == 0 {
		return slotRef{}, false
	}
	last := len(m.refs) - 1
	ref := m.refs[last]
	m.refs[last] = slotRef{}
	m.refs = m.refs[:last]
	return ref, true
}

// IsZero reports whether h cannot reference an active slot.
func (h InternalTrackerHandle) IsZero() bool {
	return h.slotRef == nil || h.segmentRef == nil || h.generation == 0
}

// CompletedOperation is a worker-side view over a completed operation slot.
// Records is valid only for the duration of the DrainCompletedShard callback.
type CompletedOperation struct {
	Operation      apiobs.InternalOperationIdentity
	Parent         apiobs.ParentRef
	ContextVersion apiobs.ConnectionContextVersion
	// ContextOverlay contains command-scoped metadata borrowed from the operation
	// slot. It is valid only during the DrainCompletedShard callback.
	ContextOverlay map[string]string
	Status         SlotTerminalStatus
	Records        []apiobs.TelemetryRecord
	DroppedRecords uint64
}

// OperationSnapshotMetadata is fixed identity metadata stored with an active
// slot so replay/crashdump can build an off-path projection without depending
// on later telemetry records having already been published.
type OperationSnapshotMetadata struct {
	Type          string
	Ref           apiobs.OperationRef
	StartUnixNano int64
}

// ActiveOperationSnapshot is a worker-side materialized view of an in-flight
// operation. It is allocated only by diagnostics/replay paths, never by the
// command telemetry submit path.
type ActiveOperationSnapshot struct {
	Operation     apiobs.InternalOperationIdentity
	Ref           apiobs.OperationRef
	Type          string
	Parent        apiobs.ParentRef
	StartUnixNano int64
	Context       map[string]string
}

type activeOperationSnapshotInput struct {
	Operation      apiobs.InternalOperationIdentity
	Metadata       OperationSnapshotMetadata
	Parent         apiobs.ParentRef
	ContextVersion apiobs.ConnectionContextVersion
	ContextOverlay map[string]string
	Records        []apiobs.TelemetryRecord
}

// SlotShardStats reports shard capacity and pressure counters.
type SlotShardStats struct {
	Segments       int
	FreeSlots      int
	ActiveSlots    int
	CompletedSlots int
}

// SlotOperationTrackerManager stores operation telemetry in preallocated shards
// and slot segments. It is the ADR-0034 storage primitive; server projection and
// GCPC/log/event materialization stay outside this type.
type SlotOperationTrackerManager struct {
	shards          []operationSlotShard
	releaseContext  func(apiobs.ConnectionContextVersion) bool
	contexts        connectionContextStore
	notifyCompleted func()

	hotShardGrowthStop     chan struct{}
	hotShardGrowthDone     chan struct{}
	hotShardGrowthStopOnce sync.Once

	skippedOperations uint64
	droppedRecords    uint64
	droppedCompleted  uint64
	invalidHandles    uint64
}

type operationSlotShard struct {
	mu sync.Mutex

	segments            []atomic.Pointer[operationSegment]
	minSegments         int
	maxSegments         int
	segmentSize         int
	recordsPerOperation int
	magazineCapacity    int

	free         []slotRef
	freeCount    int
	completed    *completedRing
	activeSlots  atomic.Int32
	freeSlots    atomic.Int32
	segmentSlots atomic.Int32
	skipped      atomic.Uint64
}

type slotRef struct {
	segment    int
	slot       int
	segmentRef *operationSegment
}

type operationSegment struct {
	index               int
	slots               []operationSlot
	records             []apiobs.TelemetryRecord
	recordsPerOperation int
	active              atomic.Int32
	retiring            atomic.Bool
}

type operationSlotState uint8

const (
	operationSlotFree operationSlotState = iota
	operationSlotActive
	operationSlotTerminal
	operationSlotWorkerOwned
)

type operationSlot struct {
	generation            uint32
	state                 atomic.Uint32
	operation             apiobs.InternalOperationIdentity
	parent                apiobs.ParentRef
	contextVersion        apiobs.ConnectionContextVersion
	contextOverlay        map[string]string
	status                SlotTerminalStatus
	snapshotTypeLen       uint16
	snapshotType          [apiobs.TelemetryNameBytes]byte
	snapshotIDLen         uint16
	snapshotID            [apiobs.TelemetryNameBytes]byte
	snapshotParentIDLen   uint16
	snapshotParentID      [apiobs.TelemetryParentIDBytes]byte
	snapshotStartUnixNano int64
	recordCount           atomic.Uint32
	droppedRecords        uint64
}

type completedRing struct {
	mask     uint64
	capacity uint64
	slots    []completedRingSlot
	_        [cacheLineBytes - 8]byte
	head     uint64
	_        [cacheLineBytes - 8]byte
	tail     uint64
	_        [cacheLineBytes - 8]byte
	drops    uint64
}

type completedRingSlot struct {
	sequence uint64
	ref      slotRef
}

func newCompletedRing(capacity int) *completedRing {
	if capacity < 1 {
		capacity = 1
	}
	size := 1
	for size < capacity {
		size <<= 1
	}
	if size < 2 {
		size = 2
	}
	ring := &completedRing{mask: uint64(size - 1), capacity: uint64(capacity), slots: make([]completedRingSlot, size)}
	for i := range ring.slots {
		ring.slots[i].sequence = uint64(i)
	}
	return ring
}

func (r *completedRing) push(ref slotRef) bool {
	pos := atomic.LoadUint64(&r.tail)
	for {
		if pos-atomic.LoadUint64(&r.head) >= r.capacity {
			atomic.AddUint64(&r.drops, 1)
			return false
		}
		slot := &r.slots[pos&r.mask]
		sequence := atomic.LoadUint64(&slot.sequence)
		delta := int64(sequence) - int64(pos)
		if delta == 0 {
			if atomic.CompareAndSwapUint64(&r.tail, pos, pos+1) {
				slot.ref = ref
				atomic.StoreUint64(&slot.sequence, pos+1)
				return true
			}
			pos = atomic.LoadUint64(&r.tail)
			continue
		}
		if delta < 0 {
			atomic.AddUint64(&r.drops, 1)
			return false
		}
		pos = atomic.LoadUint64(&r.tail)
	}
}

func (r *completedRing) pop() (slotRef, bool) {
	pos := atomic.LoadUint64(&r.head)
	for {
		slot := &r.slots[pos&r.mask]
		sequence := atomic.LoadUint64(&slot.sequence)
		delta := int64(sequence) - int64(pos+1)
		if delta == 0 {
			if atomic.CompareAndSwapUint64(&r.head, pos, pos+1) {
				ref := slot.ref
				slot.ref = slotRef{}
				atomic.StoreUint64(&slot.sequence, pos+r.mask+1)
				return ref, true
			}
			pos = atomic.LoadUint64(&r.head)
			continue
		}
		if delta < 0 {
			return slotRef{}, false
		}
		pos = atomic.LoadUint64(&r.head)
	}
}

func (r *completedRing) count() int {
	head := atomic.LoadUint64(&r.head)
	tail := atomic.LoadUint64(&r.tail)
	if tail < head {
		return 0
	}
	count := tail - head
	if count > r.capacity {
		return int(r.capacity)
	}
	return int(count)
}

// NewSlotOperationTrackerManager returns preallocated shard-owned operation-slot
// storage. Growth beyond the minimum must be triggered explicitly by background
// code with GrowShard; StartOperation never allocates when no slot is free.
func NewSlotOperationTrackerManager(config SlotTrackerConfig) *SlotOperationTrackerManager {
	config = normalizeSlotTrackerConfig(config)
	manager := &SlotOperationTrackerManager{
		shards:         make([]operationSlotShard, config.ShardCount),
		releaseContext: config.ReleaseContextVersionFn,
	}
	for i := range manager.shards {
		manager.shards[i].init(config)
	}
	manager.contexts.init()
	manager.startHotShardGrowthMonitor(config.HotShardGrowth)
	return manager
}

func normalizeSlotTrackerConfig(config SlotTrackerConfig) SlotTrackerConfig {
	if config.ShardCount < 1 {
		config.ShardCount = 1
	}
	if config.MinSegmentsPerShard < 1 {
		config.MinSegmentsPerShard = 1
	}
	if config.MaxSegmentsPerShard < config.MinSegmentsPerShard {
		config.MaxSegmentsPerShard = config.MinSegmentsPerShard
	}
	if config.SegmentSize < 1 {
		config.SegmentSize = 1
	}
	if config.MagazineCapacity < 1 {
		config.MagazineCapacity = defaultSlotMagazineCapacity
	}
	if config.RecordsPerOperation < 1 {
		config.RecordsPerOperation = 1
	}
	if config.CompletedRingPerShard < 1 {
		config.CompletedRingPerShard = 1
	}
	config.HotShardGrowth = normalizeHotShardGrowthConfig(config.HotShardGrowth)
	return config
}

func normalizeHotShardGrowthConfig(config HotShardGrowthConfig) HotShardGrowthConfig {
	if config.OccupancyThreshold <= 0 || config.OccupancyThreshold > 1 {
		config.OccupancyThreshold = defaultHotShardGrowthOccupancyThreshold
	}
	if config.SustainedChecks < 1 {
		config.SustainedChecks = defaultHotShardGrowthSustainedChecks
	}
	if config.MaxGrowthSegments < 1 {
		config.MaxGrowthSegments = defaultHotShardGrowthMaxSegments
	}
	if config.ShrinkThreshold <= 0 || config.ShrinkThreshold >= config.OccupancyThreshold {
		config.ShrinkThreshold = defaultHotShardGrowthShrinkThreshold
		if config.ShrinkThreshold >= config.OccupancyThreshold {
			config.ShrinkThreshold = config.OccupancyThreshold / 2
		}
	}
	if config.CheckInterval <= 0 {
		config.CheckInterval = defaultHotShardGrowthCheckInterval
	}
	return config
}

func (s *operationSlotShard) init(config SlotTrackerConfig) {
	s.minSegments = config.MinSegmentsPerShard
	s.maxSegments = config.MaxSegmentsPerShard
	s.segmentSize = config.SegmentSize
	s.recordsPerOperation = config.RecordsPerOperation
	s.magazineCapacity = config.MagazineCapacity
	s.segments = make([]atomic.Pointer[operationSegment], config.MaxSegmentsPerShard)
	s.free = make([]slotRef, config.MaxSegmentsPerShard*config.SegmentSize)
	s.completed = newCompletedRing(config.CompletedRingPerShard)
	for range config.MinSegmentsPerShard {
		s.addSegmentLocked()
	}
}

// Close stops optional background monitors owned by the slot tracker manager.
func (m *SlotOperationTrackerManager) Close() {
	m.StopHotShardGrowthMonitor()
}

// StopHotShardGrowthMonitor stops the optional hot-shard growth monitor. It is
// idempotent so callers can tie it to broader server or worker shutdown paths.
func (m *SlotOperationTrackerManager) StopHotShardGrowthMonitor() {
	if m == nil || m.hotShardGrowthStop == nil || m.hotShardGrowthDone == nil {
		return
	}
	m.hotShardGrowthStopOnce.Do(func() {
		close(m.hotShardGrowthStop)
	})
	<-m.hotShardGrowthDone
}

func (m *SlotOperationTrackerManager) startHotShardGrowthMonitor(config HotShardGrowthConfig) {
	if m == nil || !config.Enabled {
		return
	}
	m.hotShardGrowthStop = make(chan struct{})
	m.hotShardGrowthDone = make(chan struct{})
	go m.runHotShardGrowthMonitor(config)
}

func (m *SlotOperationTrackerManager) runHotShardGrowthMonitor(config HotShardGrowthConfig) {
	defer close(m.hotShardGrowthDone)
	ticker := time.NewTicker(config.CheckInterval)
	defer ticker.Stop()

	hotChecks := make([]int, len(m.shards))
	coldChecks := make([]int, len(m.shards))
	for {
		select {
		case <-m.hotShardGrowthStop:
			return
		case <-ticker.C:
			m.checkHotShardGrowthPressure(config, hotChecks, coldChecks)
		}
	}
}

func (m *SlotOperationTrackerManager) checkHotShardGrowthPressure(config HotShardGrowthConfig, hotChecks, coldChecks []int) {
	for shardIndex := range m.shards {
		shardStats := m.ShardStats(shardIndex)
		occupancy := slotShardOccupancy(shardStats, m.shards[shardIndex].segmentSize)
		switch {
		case occupancy >= config.OccupancyThreshold:
			coldChecks[shardIndex] = 0
			hotChecks[shardIndex]++
			if hotChecks[shardIndex] >= config.SustainedChecks && m.canGrowHotShard(shardIndex, shardStats, config) {
				if m.GrowShard(shardIndex) {
					hotChecks[shardIndex] = 0
				}
			}
		case occupancy <= config.ShrinkThreshold:
			hotChecks[shardIndex] = 0
			coldChecks[shardIndex]++
			if coldChecks[shardIndex] >= config.SustainedChecks && m.canRetireColdShardSegment(shardIndex, shardStats) {
				if m.RetireFreeSegment(shardIndex) {
					coldChecks[shardIndex] = 0
				}
			}
		default:
			hotChecks[shardIndex] = 0
			coldChecks[shardIndex] = 0
		}
	}
}

func slotShardOccupancy(shardStats SlotShardStats, segmentSize int) float64 {
	if shardStats.Segments < 1 || segmentSize < 1 {
		return 0
	}
	totalSlots := shardStats.Segments * segmentSize
	if totalSlots < 1 {
		return 0
	}
	occupiedSlots := shardStats.ActiveSlots + shardStats.CompletedSlots
	if occupiedSlots < 0 {
		return 0
	}
	if occupiedSlots > totalSlots {
		occupiedSlots = totalSlots
	}
	return float64(occupiedSlots) / float64(totalSlots)
}

func (m *SlotOperationTrackerManager) canGrowHotShard(shardIndex int, shardStats SlotShardStats, config HotShardGrowthConfig) bool {
	if shardIndex < 0 || shardIndex >= len(m.shards) {
		return false
	}
	return shardStats.Segments < m.hotShardGrowthSegmentLimit(shardIndex, config)
}

func (m *SlotOperationTrackerManager) canRetireColdShardSegment(shardIndex int, shardStats SlotShardStats) bool {
	if shardIndex < 0 || shardIndex >= len(m.shards) {
		return false
	}
	return shardStats.Segments > m.shards[shardIndex].minSegments
}

func (m *SlotOperationTrackerManager) hotShardGrowthSegmentLimit(shardIndex int, config HotShardGrowthConfig) int {
	shard := &m.shards[shardIndex]
	growthLimit := shard.minSegments * config.MaxGrowthSegments
	if growthLimit < shard.minSegments {
		growthLimit = shard.minSegments
	}
	if growthLimit > shard.maxSegments {
		return shard.maxSegments
	}
	return growthLimit
}

// ShardCount returns the configured number of slot-storage shards.
func (m *SlotOperationTrackerManager) ShardCount() int {
	return len(m.shards)
}

// FlushMagazine returns any free slots cached by a connection-owned magazine to
// the selected shard. Nil managers, nil magazines, and invalid shard indexes are
// ignored so connection teardown can call this unconditionally.
func (m *SlotOperationTrackerManager) FlushMagazine(shardIndex int, magazine *SlotMagazine) {
	if m == nil || magazine == nil || shardIndex < 0 || shardIndex >= len(m.shards) {
		return
	}
	magazine.flushToShard(&m.shards[shardIndex])
}

func (magazine *SlotMagazine) flushToShard(shard *operationSlotShard) {
	if magazine == nil || shard == nil || len(magazine.refs) == 0 {
		return
	}
	shard.mu.Lock()
	defer shard.mu.Unlock()
	for _, ref := range magazine.refs {
		if shard.freeCount >= len(shard.free) {
			break
		}
		if !shard.validFreeRefLocked(ref) {
			continue
		}
		shard.free[shard.freeCount] = ref
		shard.freeCount++
	}
	shard.freeSlots.Store(int32(shard.freeCount))
	clear(magazine.refs)
	magazine.refs = magazine.refs[:0]
}

// SetCompletedNotify wires a non-blocking notification hook invoked when a
// completed operation is successfully queued for worker drain.
func (m *SlotOperationTrackerManager) SetCompletedNotify(fn func()) {
	if m == nil {
		return
	}
	m.notifyCompleted = fn
}

// StartOperation reserves a preallocated operation slot and returns its internal
// tracker handle. It returns false when the shard has no free slot.
func (m *SlotOperationTrackerManager) StartOperation(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, contextVersion apiobs.ConnectionContextVersion) (InternalTrackerHandle, bool) {
	return m.startOperation(operation, parent, 0, contextVersion, nil, OperationSnapshotMetadata{}, nil)
}

// StartOperationWithMetadata reserves a preallocated operation slot and stores
// fixed replay/crashdump identity metadata with the slot. The metadata is copied
// into inline storage and does not materialize maps, logs, events, or GCPC data.
func (m *SlotOperationTrackerManager) StartOperationWithMetadata(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, contextVersion apiobs.ConnectionContextVersion, metadata OperationSnapshotMetadata, magazines ...*SlotMagazine) (InternalTrackerHandle, bool) {
	return m.startOperation(operation, parent, 0, contextVersion, nil, metadata, firstSlotMagazine(magazines))
}

func shardIndexForConnection(connection apiobs.ConnectionIdentity, operation apiobs.InternalOperationIdentity, shardCount int) int {
	if connection != 0 {
		return int(uint64(connection) % uint64(shardCount))
	}
	return shardIndex(operation, shardCount)
}

func (m *SlotOperationTrackerManager) startOperation(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, connection apiobs.ConnectionIdentity, contextVersion apiobs.ConnectionContextVersion, contextOverlay map[string]string, metadata OperationSnapshotMetadata, magazine *SlotMagazine) (InternalTrackerHandle, bool) {
	shardIndex := shardIndexForConnection(connection, operation, len(m.shards))
	shard := &m.shards[shardIndex]

	if magazine != nil {
		for {
			ref, ok := magazine.pop()
			if !ok {
				break
			}
			if handle, ok := m.initSlotFromRef(shard, shardIndex, ref, operation, parent, contextVersion, contextOverlay, metadata); ok {
				return handle, true
			}
		}
	}

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if magazine != nil && len(magazine.refs) == 0 && shard.freeCount > 0 {
		batch := min(shard.magazineCapacity, shard.freeCount)
		if cap(magazine.refs) < batch {
			magazine.refs = make([]slotRef, 0, batch)
		} else {
			magazine.refs = magazine.refs[:0]
		}
		for i := 0; i < batch; i++ {
			shard.freeCount--
			magazine.refs = append(magazine.refs, shard.free[shard.freeCount])
			shard.free[shard.freeCount] = slotRef{}
		}
		shard.freeSlots.Store(int32(shard.freeCount))
		for {
			ref, ok := magazine.pop()
			if !ok {
				break
			}
			if handle, ok := m.initSlotFromRef(shard, shardIndex, ref, operation, parent, contextVersion, contextOverlay, metadata); ok {
				return handle, true
			}
		}
	}

	for shard.freeCount > 0 {
		shard.freeCount--
		ref := shard.free[shard.freeCount]
		shard.free[shard.freeCount] = slotRef{}
		shard.freeSlots.Store(int32(shard.freeCount))
		if handle, ok := m.initSlotFromRef(shard, shardIndex, ref, operation, parent, contextVersion, contextOverlay, metadata); ok {
			return handle, true
		}
	}

	atomic.AddUint64(&m.skippedOperations, 1)
	shard.skipped.Add(1)
	return InternalTrackerHandle{}, false
}

func (m *SlotOperationTrackerManager) initSlotFromRef(shard *operationSlotShard, shardIndex int, ref slotRef, operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, contextVersion apiobs.ConnectionContextVersion, contextOverlay map[string]string, metadata OperationSnapshotMetadata) (InternalTrackerHandle, bool) {
	segment, ok := shard.reserveSegmentForRef(ref)
	if !ok {
		return InternalTrackerHandle{}, false
	}
	slot := &segment.slots[ref.slot]
	if operationSlotState(slot.state.Load()) != operationSlotFree {
		segment.active.Add(-1)
		return InternalTrackerHandle{}, false
	}
	slot.generation++
	if slot.generation == 0 {
		slot.generation++
	}
	slot.operation = operation
	slot.parent = parent
	slot.contextVersion = contextVersion
	slot.contextOverlay = contextOverlay
	slot.status = SlotTerminalUnknown
	slot.setSnapshotMetadata(metadata)
	slot.recordCount.Store(0)
	slot.droppedRecords = 0
	slot.state.Store(uint32(operationSlotActive))
	shard.activeSlots.Add(1)

	return InternalTrackerHandle{
		shard:      uint16(shardIndex),
		segment:    uint16(ref.segment),
		slot:       uint32(ref.slot),
		generation: slot.generation,
		segmentRef: segment,
		slotRef:    slot,
	}, true
}

func firstSlotMagazine(magazines []*SlotMagazine) *SlotMagazine {
	if len(magazines) == 0 {
		return nil
	}
	return magazines[0]
}

// StartOperationForConnection pins the current connection-context version and
// reserves an operation slot. If no slot is available, the pinned version is
// released before returning.
func (m *SlotOperationTrackerManager) StartOperationForConnection(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, connection apiobs.ConnectionIdentity) (InternalTrackerHandle, apiobs.ConnectionContextVersion, bool) {
	return m.StartOperationForConnectionWithMetadata(operation, parent, connection, OperationSnapshotMetadata{}, nil)
}

// StartOperationForConnectionWithMetadata pins the current connection-context
// version and reserves an operation slot with fixed replay/crashdump metadata.
func (m *SlotOperationTrackerManager) StartOperationForConnectionWithMetadata(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, connection apiobs.ConnectionIdentity, metadata OperationSnapshotMetadata, magazines ...*SlotMagazine) (InternalTrackerHandle, apiobs.ConnectionContextVersion, bool) {
	version := m.PinCurrentConnectionContextVersion(connection)
	magazine := firstSlotMagazine(magazines)
	handle, ok := m.startOperation(operation, parent, connection, version, nil, metadata, magazine)
	if !ok {
		m.ReleaseConnectionContextVersion(version)
		return InternalTrackerHandle{}, 0, false
	}
	return handle, version, true
}

// StartOperationWithConnectionContext pins the current connection context and
// attaches command-scoped values as an operation-local overlay. The overlay is
// not installed as the connection's current base, so command metadata applies
// only to this operation. Callers must not mutate pairs until the operation is
// drained.
func (m *SlotOperationTrackerManager) StartOperationWithConnectionContext(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, connection apiobs.ConnectionIdentity, pairs map[string]string) (InternalTrackerHandle, apiobs.ConnectionContextVersion, bool) {
	return m.StartOperationWithConnectionContextAndMetadata(operation, parent, connection, pairs, OperationSnapshotMetadata{}, nil)
}

// StartOperationWithConnectionContextAndMetadata pins the current connection
// context, attaches command-scoped overlay, and stores fixed replay/crashdump
// metadata with the slot.
func (m *SlotOperationTrackerManager) StartOperationWithConnectionContextAndMetadata(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, connection apiobs.ConnectionIdentity, pairs map[string]string, metadata OperationSnapshotMetadata, magazines ...*SlotMagazine) (InternalTrackerHandle, apiobs.ConnectionContextVersion, bool) {
	magazine := firstSlotMagazine(magazines)
	if len(pairs) == 0 {
		return m.StartOperationForConnectionWithMetadata(operation, parent, connection, metadata, magazine)
	}
	version := m.PinCurrentConnectionContextVersion(connection)
	handle, ok := m.startOperation(operation, parent, connection, version, pairs, metadata, magazine)
	if !ok {
		m.ReleaseConnectionContextVersion(version)
		return InternalTrackerHandle{}, 0, false
	}
	return handle, version, true
}

// StartOperationWithPinnedConnectionContextAndMetadata starts an operation with
// a version the caller has already pinned. On success the slot owns that pin and
// releases it during drain/reset; on failure the pin is released before return.
func (m *SlotOperationTrackerManager) StartOperationWithPinnedConnectionContextAndMetadata(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, connection apiobs.ConnectionIdentity, version apiobs.ConnectionContextVersion, pairs map[string]string, metadata OperationSnapshotMetadata, magazines ...*SlotMagazine) (InternalTrackerHandle, bool) {
	magazine := firstSlotMagazine(magazines)
	handle, ok := m.startOperation(operation, parent, connection, version, pairs, metadata, magazine)
	if !ok {
		m.ReleaseConnectionContextVersion(version)
		return InternalTrackerHandle{}, false
	}
	return handle, true
}

// OperationContextSnapshot returns the active operation's materialized context:
// the pinned connection base, command overlay, and operation-local context
// update/remove records folded in record order. The returned map is owned by the
// caller. It is intended for boundary projection paths such as GCPC; the command
// no-sink path must not call it.
func (m *SlotOperationTrackerManager) OperationContextSnapshot(handle InternalTrackerHandle) map[string]string {
	if m == nil || handle.IsZero() || int(handle.shard) >= len(m.shards) {
		return nil
	}
	shard := &m.shards[handle.shard]

	shard.mu.Lock()
	segment, slot, ok := shard.activeSlotLocked(handle)
	if !ok {
		shard.mu.Unlock()
		return nil
	}
	contextVersion := slot.contextVersion
	contextOverlay := slot.contextOverlay
	recordCount := int(slot.recordCount.Load())
	start := int(handle.slot) * segment.recordsPerOperation
	records := append([]apiobs.TelemetryRecord(nil), segment.records[start:start+recordCount]...)
	shard.mu.Unlock()

	return m.materializeOperationContext(contextVersion, contextOverlay, records)
}

// CompletedOperationContext returns a completed operation's materialized context
// using the same base/overlay/record folding order as OperationContextSnapshot.
// The returned map is owned by the caller.
func (m *SlotOperationTrackerManager) CompletedOperationContext(operation CompletedOperation) map[string]string {
	if m == nil {
		return nil
	}
	return m.materializeOperationContext(operation.ContextVersion, operation.ContextOverlay, operation.Records)
}

// ActiveOperationSnapshots returns materialized snapshots of active operations.
// It is intended for replay, crashdump, and diagnostics paths; command telemetry
// submission must not call it.
func (m *SlotOperationTrackerManager) ActiveOperationSnapshots() []ActiveOperationSnapshot {
	if m == nil {
		return nil
	}
	var inputs []activeOperationSnapshotInput
	for shardIndex := range m.shards {
		inputs = m.appendActiveOperationSnapshotInputs(inputs, shardIndex)
	}
	if len(inputs) == 0 {
		return nil
	}
	snapshots := make([]ActiveOperationSnapshot, 0, len(inputs))
	for _, input := range inputs {
		operationContext := m.materializeOperationContext(input.ContextVersion, input.ContextOverlay, input.Records)
		snapshots = append(snapshots, activeOperationSnapshotFromContext(input, operationContext))
	}
	return snapshots
}

func (m *SlotOperationTrackerManager) appendActiveOperationSnapshotInputs(inputs []activeOperationSnapshotInput, shardIndex int) []activeOperationSnapshotInput {
	shard := &m.shards[shardIndex]
	shard.mu.Lock()
	defer shard.mu.Unlock()

	for i := range shard.segments {
		segment := shard.segments[i].Load()
		if segment == nil {
			continue
		}
		for slotIndex := range segment.slots {
			slot := &segment.slots[slotIndex]
			if operationSlotState(slot.state.Load()) != operationSlotActive {
				continue
			}
			start := slotIndex * segment.recordsPerOperation
			end := start + int(slot.recordCount.Load())
			input := activeOperationSnapshotInput{
				Operation:      slot.operation,
				Metadata:       slot.snapshotMetadata(),
				Parent:         slot.parent,
				ContextVersion: slot.contextVersion,
				ContextOverlay: cloneStringMap(slot.contextOverlay),
			}
			if end > start {
				input.Records = append([]apiobs.TelemetryRecord(nil), segment.records[start:end]...)
			}
			inputs = append(inputs, input)
		}
	}
	return inputs
}

func activeOperationSnapshotFromContext(input activeOperationSnapshotInput, operationContext map[string]string) ActiveOperationSnapshot {
	start := activeOperationStartProjection(input.Records)
	operationID := firstNonEmpty(
		input.Metadata.Ref.ID.String(),
		operationContext[apicommand.OperationID],
		start.fields[apicommand.OperationID],
		strconv.FormatInt(int64(input.Operation), 10),
	)
	parentID := firstNonEmpty(
		input.Metadata.Ref.ParentID.String(),
		operationContext["_parent_operation_id"],
		start.fields["_parent_operation_id"],
		input.Parent.String(),
	)
	operationType := firstNonEmpty(
		input.Metadata.Type,
		operationContext["_operation_type"],
		start.operationType,
		commandOperationType(operationContext),
	)
	return ActiveOperationSnapshot{
		Operation:     input.Operation,
		Ref:           apiobs.NewOperationRef(operationID, parentID),
		Type:          operationType,
		Parent:        input.Parent,
		StartUnixNano: firstNonZeroInt64(input.Metadata.StartUnixNano, parseInt64(operationContext[apicommand.StartNs]), start.startUnixNano),
		Context:       operationContext,
	}
}

type activeOperationStart struct {
	operationType string
	startUnixNano int64
	fields        map[string]string
}

func activeOperationStartProjection(records []apiobs.TelemetryRecord) activeOperationStart {
	for i := range records {
		record := records[i]
		if record.Kind != apiobs.TelemetryRecordOperationStart {
			continue
		}
		return activeOperationStart{
			operationType: string(record.NameBytes()),
			startUnixNano: record.TimestampUnixNano,
			fields:        telemetryRecordFields(record),
		}
	}
	return activeOperationStart{}
}

func commandOperationType(operationContext map[string]string) string {
	if operationContext[apicommand.CommandKey] == "" {
		return ""
	}
	return "command"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZeroInt64(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func parseInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func telemetryRecordFields(record apiobs.TelemetryRecord) map[string]string {
	payload := record.PayloadBytes()
	if len(payload) == 0 {
		return nil
	}
	fields := make(map[string]string)
	pos := 1
	for count := int(payload[0]); count > 0; count-- {
		if pos >= len(payload) {
			return fields
		}
		keyLen := int(payload[pos])
		pos++
		if pos+keyLen > len(payload) {
			return fields
		}
		key := string(payload[pos : pos+keyLen])
		pos += keyLen
		if pos >= len(payload) {
			return fields
		}
		valueLen := int(payload[pos])
		pos++
		if pos+valueLen > len(payload) {
			return fields
		}
		fields[key] = string(payload[pos : pos+valueLen])
		pos += valueLen
	}
	return fields
}

func (m *SlotOperationTrackerManager) materializeOperationContext(contextVersion apiobs.ConnectionContextVersion, contextOverlay map[string]string, records []apiobs.TelemetryRecord) map[string]string {
	operationContext := m.copyContextVersion(contextVersion)
	for key, value := range contextOverlay {
		if operationContext == nil {
			operationContext = make(map[string]string, len(contextOverlay))
		}
		operationContext[key] = value
	}
	return foldContextRecords(operationContext, records)
}

func (m *SlotOperationTrackerManager) copyContextVersion(contextVersion apiobs.ConnectionContextVersion) map[string]string {
	if contextVersion.IsZero() {
		return nil
	}
	var operationContext map[string]string
	m.VisitConnectionContextVersion(contextVersion, func(key, value string) bool {
		if operationContext == nil {
			operationContext = make(map[string]string)
		}
		operationContext[key] = value
		return true
	})
	return operationContext
}

func foldContextRecords(operationContext map[string]string, records []apiobs.TelemetryRecord) map[string]string {
	for i := range records {
		switch records[i].Kind {
		case apiobs.TelemetryRecordContextUpdate:
			operationContext = foldContextUpdate(operationContext, records[i])
		case apiobs.TelemetryRecordContextRemove:
			foldContextRemove(operationContext, records[i])
		}
	}
	return operationContext
}

func foldContextUpdate(operationContext map[string]string, record apiobs.TelemetryRecord) map[string]string {
	payload := record.PayloadBytes()
	if len(payload) == 0 {
		return operationContext
	}
	pos := 1
	for count := int(payload[0]); count > 0; count-- {
		if pos >= len(payload) {
			return operationContext
		}
		keyLen := int(payload[pos])
		pos++
		if pos+keyLen > len(payload) {
			return operationContext
		}
		key := string(payload[pos : pos+keyLen])
		pos += keyLen
		if pos >= len(payload) {
			return operationContext
		}
		valueLen := int(payload[pos])
		pos++
		if pos+valueLen > len(payload) {
			return operationContext
		}
		value := string(payload[pos : pos+valueLen])
		pos += valueLen
		if operationContext == nil {
			operationContext = make(map[string]string, count)
		}
		operationContext[key] = value
	}
	return operationContext
}

func foldContextRemove(operationContext map[string]string, record apiobs.TelemetryRecord) {
	if len(operationContext) == 0 {
		return
	}
	payload := record.PayloadBytes()
	if len(payload) == 0 {
		return
	}
	pos := 1
	for count := int(payload[0]); count > 0; count-- {
		if pos >= len(payload) {
			return
		}
		keyLen := int(payload[pos])
		pos++
		if pos+keyLen > len(payload) {
			return
		}
		delete(operationContext, string(payload[pos:pos+keyLen]))
		pos += keyLen
	}
}

// RecordTelemetry appends record to the operation-local fixed record storage.
func (m *SlotOperationTrackerManager) RecordTelemetry(handle InternalTrackerHandle, record apiobs.TelemetryRecord) bool {
	segment, slot, ok := m.activeSlot(handle)
	if !ok {
		atomic.AddUint64(&m.invalidHandles, 1)
		return false
	}
	count := int(slot.recordCount.Load())
	if count >= segment.recordsPerOperation {
		slot.droppedRecords++
		atomic.AddUint64(&m.droppedRecords, 1)
		return false
	}
	record.Operation = slot.operation
	segment.records[int(handle.slot)*segment.recordsPerOperation+count] = record
	slot.recordCount.Store(uint32(count + 1))
	return true
}

// FinishOperation marks an active slot terminal and enqueues it for worker drain.
func (m *SlotOperationTrackerManager) FinishOperation(handle InternalTrackerHandle, status SlotTerminalStatus) bool {
	if int(handle.shard) >= len(m.shards) {
		atomic.AddUint64(&m.invalidHandles, 1)
		return false
	}
	shard := &m.shards[handle.shard]
	segment := handle.segmentRef
	slot := handle.slotRef
	if segment == nil || slot == nil || operationSlotState(slot.state.Load()) != operationSlotActive {
		atomic.AddUint64(&m.invalidHandles, 1)
		return false
	}
	if int(handle.segment) >= len(shard.segments) || shard.segments[handle.segment].Load() != segment {
		atomic.AddUint64(&m.invalidHandles, 1)
		return false
	}
	if int(handle.slot) >= len(segment.slots) || slot != &segment.slots[handle.slot] || slot.generation != handle.generation {
		atomic.AddUint64(&m.invalidHandles, 1)
		return false
	}
	if status == SlotTerminalUnknown {
		status = SlotTerminalFinished
	}
	if !slot.state.CompareAndSwap(uint32(operationSlotActive), uint32(operationSlotTerminal)) {
		atomic.AddUint64(&m.invalidHandles, 1)
		return false
	}
	slot.status = status
	ref := slotRef{segment: int(handle.segment), slot: int(handle.slot), segmentRef: segment}
	if !shard.completed.push(ref) {
		atomic.AddUint64(&m.droppedCompleted, 1)
		m.releaseContextVersion(slot.contextVersion)
		shard.mu.Lock()
		shard.resetSlotLocked(segment, ref)
		shard.mu.Unlock()
		return false
	} else if m.notifyCompleted != nil {
		m.notifyCompleted()
	}
	return true
}

// DrainCompletedShard drains terminal slots for worker-side processing.
func (m *SlotOperationTrackerManager) DrainCompletedShard(index int, fn func(CompletedOperation)) int {
	if index < 0 || index >= len(m.shards) {
		return 0
	}
	shard := &m.shards[index]
	var operations []CompletedOperation
	var refs []slotRef
	for {
		ref, ok := shard.completed.pop()
		if !ok {
			break
		}
		segment, ok := shard.segmentForRef(ref)
		if !ok {
			atomic.AddUint64(&m.invalidHandles, 1)
			continue
		}
		slot := &segment.slots[ref.slot]
		if operationSlotState(slot.state.Load()) != operationSlotTerminal {
			atomic.AddUint64(&m.invalidHandles, 1)
			continue
		}
		slot.state.Store(uint32(operationSlotWorkerOwned))
		operations = append(operations, completedOperationFromSlot(segment, slot, ref.slot))
		refs = append(refs, ref)
	}
	if len(operations) == 0 {
		return 0
	}
	for i := range operations {
		if fn != nil {
			fn(operations[i])
		}
	}
	shard.mu.Lock()
	for i := range refs {
		ref := refs[i]
		segment, ok := shard.segmentForRef(ref)
		if !ok {
			continue
		}
		slot := &segment.slots[ref.slot]
		m.releaseContextVersion(slot.contextVersion)
		shard.resetSlotLocked(segment, ref)
	}
	shard.mu.Unlock()
	return len(operations)
}

// GrowShard adds one preallocated segment to shard index. It is intended for a
// background pressure controller, not the command path.
func (m *SlotOperationTrackerManager) GrowShard(index int) bool {
	if index < 0 || index >= len(m.shards) {
		return false
	}
	shard := &m.shards[index]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	return shard.addSegmentLocked()
}

// RetireFreeSegment removes one fully free segment from shard index. It is
// intended for a background shrink controller, not the command path.
func (m *SlotOperationTrackerManager) RetireFreeSegment(index int) bool {
	if index < 0 || index >= len(m.shards) {
		return false
	}
	shard := &m.shards[index]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	return shard.retireFreeSegmentLocked()
}

// ShardStats returns a snapshot of shard capacity state.
func (m *SlotOperationTrackerManager) ShardStats(index int) SlotShardStats {
	if index < 0 || index >= len(m.shards) {
		return SlotShardStats{}
	}
	shard := &m.shards[index]
	return SlotShardStats{
		Segments:       int(shard.segmentSlots.Load()),
		FreeSlots:      int(shard.freeSlots.Load()),
		ActiveSlots:    int(shard.activeSlots.Load()),
		CompletedSlots: shard.completed.count(),
	}
}

// ShardSkipped returns the skip counter for a shard.
func (m *SlotOperationTrackerManager) ShardSkipped(index int) uint64 {
	if index < 0 || index >= len(m.shards) {
		return 0
	}
	return m.shards[index].skipped.Load()
}

// ShardActiveSlots returns active slot count for a shard.
func (m *SlotOperationTrackerManager) ShardActiveSlots(index int) int {
	if index < 0 || index >= len(m.shards) {
		return 0
	}
	return int(m.shards[index].activeSlots.Load())
}

// ShardFreeSlots returns free slot count for a shard.
func (m *SlotOperationTrackerManager) ShardFreeSlots(index int) int {
	if index < 0 || index >= len(m.shards) {
		return 0
	}
	return int(m.shards[index].freeSlots.Load())
}

// ShardCompletedSlots returns completed slot count for a shard.
func (m *SlotOperationTrackerManager) ShardCompletedSlots(index int) int {
	if index < 0 || index >= len(m.shards) {
		return 0
	}
	m.shards[index].mu.Lock()
	defer m.shards[index].mu.Unlock()
	return m.shards[index].completed.count()
}

// UpdateConnectionContext creates a new immutable context version for connection.
func (m *SlotOperationTrackerManager) UpdateConnectionContext(connection apiobs.ConnectionIdentity, pairs ...[]byte) apiobs.ConnectionContextVersion {
	return m.contexts.update(connection, pairs)
}

// UpdateConnectionContextStrings creates a new immutable context version for
// string-backed metadata without forcing callers to allocate temporary byte slices.
func (m *SlotOperationTrackerManager) UpdateConnectionContextStrings(connection apiobs.ConnectionIdentity, pairs ...string) apiobs.ConnectionContextVersion {
	return m.contexts.updateStrings(connection, pairs)
}

// RemoveConnectionContext creates a new immutable context version without keys.
func (m *SlotOperationTrackerManager) RemoveConnectionContext(connection apiobs.ConnectionIdentity, keys ...[]byte) apiobs.ConnectionContextVersion {
	return m.contexts.remove(connection, keys)
}

// RemoveConnectionContextStrings creates a new immutable context version without
// string-backed keys and without temporary byte-slice conversions.
func (m *SlotOperationTrackerManager) RemoveConnectionContextStrings(connection apiobs.ConnectionIdentity, keys ...string) apiobs.ConnectionContextVersion {
	return m.contexts.removeStrings(connection, keys)
}

// PinCurrentConnectionContextVersion retains the current context version for connection.
func (m *SlotOperationTrackerManager) PinCurrentConnectionContextVersion(connection apiobs.ConnectionIdentity) apiobs.ConnectionContextVersion {
	return m.contexts.pinCurrent(connection)
}

// RetainConnectionContextVersion increments the reference count for version.
func (m *SlotOperationTrackerManager) RetainConnectionContextVersion(version apiobs.ConnectionContextVersion) bool {
	return m.contexts.retain(version)
}

// ReleaseConnectionContextVersion releases a previously retained context version.
func (m *SlotOperationTrackerManager) ReleaseConnectionContextVersion(version apiobs.ConnectionContextVersion) bool {
	return m.contexts.release(version)
}

// VisitConnectionContextVersion visits immutable key/value pairs for version.
func (m *SlotOperationTrackerManager) VisitConnectionContextVersion(version apiobs.ConnectionContextVersion, visitor apiobs.ConnectionContextVisitor) bool {
	return m.contexts.visit(version, visitor)
}

// ForgetConnectionContext removes the current context version for a closed
// connection. Pinned versions remain visitable until their holders release them.
func (m *SlotOperationTrackerManager) ForgetConnectionContext(connection apiobs.ConnectionIdentity) bool {
	return m.contexts.forget(connection)
}

func (m *SlotOperationTrackerManager) SkippedOperations() uint64 {
	return atomic.LoadUint64(&m.skippedOperations)
}

func (m *SlotOperationTrackerManager) DroppedRecords() uint64 {
	return atomic.LoadUint64(&m.droppedRecords)
}

func (m *SlotOperationTrackerManager) DroppedCompletedOperations() uint64 {
	return atomic.LoadUint64(&m.droppedCompleted)
}

func (m *SlotOperationTrackerManager) InvalidHandles() uint64 {
	return atomic.LoadUint64(&m.invalidHandles)
}

func (s *operationSlot) setSnapshotMetadata(metadata OperationSnapshotMetadata) {
	s.snapshotTypeLen = uint16(copy(s.snapshotType[:], metadata.Type))
	s.snapshotIDLen = uint16(copy(s.snapshotID[:], metadata.Ref.ID.String()))
	s.snapshotParentIDLen = uint16(copy(s.snapshotParentID[:], metadata.Ref.ParentID.String()))
	if metadata.StartUnixNano != 0 {
		s.snapshotStartUnixNano = metadata.StartUnixNano
		return
	}
	s.snapshotStartUnixNano = nowUnixNano()
}

func (s *operationSlot) snapshotMetadata() OperationSnapshotMetadata {
	return OperationSnapshotMetadata{
		Type:          string(s.snapshotType[:s.snapshotTypeLen]),
		Ref:           apiobs.NewOperationRef(string(s.snapshotID[:s.snapshotIDLen]), string(s.snapshotParentID[:s.snapshotParentIDLen])),
		StartUnixNano: s.snapshotStartUnixNano,
	}
}

func (m *SlotOperationTrackerManager) activeSlot(handle InternalTrackerHandle) (*operationSegment, *operationSlot, bool) {
	if handle.IsZero() {
		return nil, nil, false
	}
	segment := handle.segmentRef
	slot := handle.slotRef
	if segment == nil || slot == nil || operationSlotState(slot.state.Load()) != operationSlotActive {
		return nil, nil, false
	}
	if int(handle.slot) >= len(segment.slots) || slot != &segment.slots[handle.slot] || slot.generation != handle.generation {
		return nil, nil, false
	}
	return segment, slot, true
}

func (m *SlotOperationTrackerManager) releaseContextVersion(version apiobs.ConnectionContextVersion) {
	if version == 0 {
		return
	}
	if m.releaseContext != nil {
		m.releaseContext(version)
	}
	m.contexts.release(version)
}

func (s *operationSlotShard) addSegmentLocked() bool {
	index := -1
	for i := range s.segments {
		if s.segments[i].Load() == nil {
			index = i
			break
		}
	}
	if index < 0 {
		return false
	}
	segment := newOperationSegment(index, s.segmentSize, s.recordsPerOperation)
	s.segments[index].Store(segment)
	s.segmentSlots.Add(1)
	for slot := range s.segmentSize {
		s.free[s.freeCount] = slotRef{segment: index, slot: slot, segmentRef: segment}
		s.freeCount++
	}
	s.freeSlots.Store(int32(s.freeCount))
	return true
}

func newOperationSegment(index, segmentSize, recordsPerOperation int) *operationSegment {
	return &operationSegment{
		index:               index,
		slots:               make([]operationSlot, segmentSize),
		records:             make([]apiobs.TelemetryRecord, segmentSize*recordsPerOperation),
		recordsPerOperation: recordsPerOperation,
	}
}

func (s *operationSlotShard) segmentForRef(ref slotRef) (*operationSegment, bool) {
	if ref.segment < 0 || ref.segment >= len(s.segments) {
		return nil, false
	}
	current := s.segments[ref.segment].Load()
	segment := ref.segmentRef
	if segment == nil {
		segment = current
	}
	if segment == nil || current != segment || ref.slot < 0 || ref.slot >= len(segment.slots) {
		return nil, false
	}
	if segment.retiring.Load() || segment.active.Load() < 0 {
		return nil, false
	}
	return segment, true
}

func (s *operationSlotShard) validFreeRefLocked(ref slotRef) bool {
	segment, ok := s.segmentForRef(ref)
	if !ok {
		return false
	}
	return operationSlotState(segment.slots[ref.slot].state.Load()) == operationSlotFree
}

func (s *operationSlotShard) reserveSegmentForRef(ref slotRef) (*operationSegment, bool) {
	segment, ok := s.segmentForRef(ref)
	if !ok {
		return nil, false
	}
	for {
		active := segment.active.Load()
		if active < 0 || segment.retiring.Load() {
			return nil, false
		}
		if segment.active.CompareAndSwap(active, active+1) {
			if s.segments[ref.segment].Load() != segment || segment.retiring.Load() {
				segment.active.Add(-1)
				return nil, false
			}
			return segment, true
		}
	}
}

func (s *operationSlotShard) activeSlotLocked(handle InternalTrackerHandle) (*operationSegment, *operationSlot, bool) {
	if handle.IsZero() || int(handle.segment) >= len(s.segments) {
		return nil, nil, false
	}
	segment := s.segments[handle.segment].Load()
	if segment == nil || segment != handle.segmentRef || int(handle.slot) >= len(segment.slots) {
		return nil, nil, false
	}
	slot := &segment.slots[handle.slot]
	if slot != handle.slotRef || slot.generation != handle.generation || operationSlotState(slot.state.Load()) != operationSlotActive {
		return nil, nil, false
	}
	return segment, slot, true
}

func completedOperationFromSlot(segment *operationSegment, slot *operationSlot, slotIndex int) CompletedOperation {
	start := slotIndex * segment.recordsPerOperation
	end := start + int(slot.recordCount.Load())
	return CompletedOperation{
		Operation:      slot.operation,
		Parent:         slot.parent,
		ContextVersion: slot.contextVersion,
		ContextOverlay: slot.contextOverlay,
		Status:         slot.status,
		Records:        segment.records[start:end],
		DroppedRecords: slot.droppedRecords,
	}
}

func (s *operationSlotShard) resetSlotLocked(segment *operationSegment, ref slotRef) {
	slot := &segment.slots[ref.slot]
	slot.operation = 0
	slot.parent = apiobs.ParentRef{}
	slot.contextVersion = 0
	slot.contextOverlay = nil
	slot.status = SlotTerminalUnknown
	slot.snapshotTypeLen = 0
	slot.snapshotIDLen = 0
	slot.snapshotParentIDLen = 0
	slot.snapshotStartUnixNano = 0
	slot.recordCount.Store(0)
	slot.droppedRecords = 0
	slot.state.Store(uint32(operationSlotFree))
	segment.active.Add(-1)
	s.activeSlots.Add(-1)
	if !segment.retiring.Load() {
		ref.segmentRef = segment
		s.free[s.freeCount] = ref
		s.freeCount++
		s.freeSlots.Store(int32(s.freeCount))
	}
}

func (s *operationSlotShard) retireFreeSegmentLocked() bool {
	if s.segmentCountLocked() <= s.minSegments {
		return false
	}
	for index := len(s.segments) - 1; index >= 0; index-- {
		segment := s.segments[index].Load()
		if segment == nil || !segment.active.CompareAndSwap(0, -1) {
			continue
		}
		segment.retiring.Store(true)
		s.removeFreeRefsForSegmentLocked(index)
		s.segments[index].Store(nil)
		s.segmentSlots.Add(-1)
		return true
	}
	return false
}

func (s *operationSlotShard) removeFreeRefsForSegmentLocked(segmentIndex int) {
	write := 0
	for read := 0; read < s.freeCount; read++ {
		if s.free[read].segment == segmentIndex {
			continue
		}
		s.free[write] = s.free[read]
		write++
	}
	for i := write; i < s.freeCount; i++ {
		s.free[i] = slotRef{}
	}
	s.freeCount = write
	s.freeSlots.Store(int32(s.freeCount))
}

func (s *operationSlotShard) segmentCountLocked() int {
	count := 0
	for i := range s.segments {
		if s.segments[i].Load() != nil {
			count++
		}
	}
	return count
}
