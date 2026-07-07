package server

import (
	"context"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apicommand "gocache/api/command"
	apictx "gocache/api/context"
	apievents "gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"
	apiobs "gocache/api/observability"
	"gocache/commons/logger"
	commonobs "gocache/commons/observability"
)

const defaultOperationTrackerDrainInterval = 10 * time.Millisecond
const defaultOperationTrackerGapInterval = 100 * time.Millisecond
const maxOperationTrackerDrainWorkerCount = 8

const operationTrackerDrainParentField = "_parent_operation_id"

// OperationTrackerDrainWorker owns server-side projection of completed
// OperationTracker slots. It drains and materializes accepted telemetry records
// after command execution has finished, keeping zerolog formatting and sink I/O
// off the command goroutine.
type OperationTrackerDrainWorker struct {
	manager                *commonobs.SlotOperationTrackerManager
	interval               time.Duration
	gapInterval            time.Duration
	emitter                apievents.Emitter
	tmpfsWriterMu          sync.RWMutex
	tmpfsWriter            io.Writer
	hasTelemetrySubscriber bool

	idleBackoff  time.Duration // exponential backoff for idle state
	workerCount  int
	rangeWorkers []shardRangeWorker
	started      atomic.Bool

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type shardRangeWorker struct {
	shardStart int
	shardEnd   int // exclusive
	drainMu    sync.Mutex
	nudgeCh    chan struct{} // buffered 1
	scratch    drainWorkerScratch
}

type drainWorkerScratch struct {
	fields     []kvPair
	args       []string
	metadata   []kvPair
	eventBuf   *gcpcv1.EventV1
	contextMap map[string]string

	// Proto object pooling eliminates per-operation telemetry graph allocations after warmup.
	telemetryOp *gcpcv1.TelemetryOperation
	protoItems  []*gcpcv1.TelemetryItem
	protoTags   []*gcpcv1.Tag
	marshalBuf  []byte
}

type gapJanitor struct {
	interval       time.Duration
	lastSkipped    uint64
	lastDropped    uint64
	lastCompleted  uint64
	lastInvalid    uint64
	lastSampleTime time.Time
}

type kvPair struct {
	key   string
	value string
}

func (s *drainWorkerScratch) reset() {
	if s == nil {
		return
	}
	s.fields = s.fields[:0]
	s.args = s.args[:0]
	s.metadata = s.metadata[:0]
	if s.eventBuf != nil {
		*s.eventBuf = gcpcv1.EventV1{}
	}
	for key := range s.contextMap {
		delete(s.contextMap, key)
	}
	if s.telemetryOp != nil {
		s.telemetryOp.OperationId = ""
		s.telemetryOp.InitialContext = s.telemetryOp.InitialContext[:0]
		s.telemetryOp.TelemetryItems = s.telemetryOp.TelemetryItems[:0]
	}
	for i := range s.protoItems {
		s.protoItems[i].Kind = 0
		s.protoItems[i].Payload = nil
	}
	for i := range s.protoTags {
		s.protoTags[i].Key = s.protoTags[i].Key[:0]
		s.protoTags[i].Value = s.protoTags[i].Value[:0]
	}
	s.marshalBuf = s.marshalBuf[:0]
}

func (s *drainWorkerScratch) borrowTag(key, value string) *gcpcv1.Tag {
	var tag *gcpcv1.Tag
	if tagCount := len(s.protoTags); tagCount > 0 {
		tag = s.protoTags[tagCount-1]
		s.protoTags = s.protoTags[:tagCount-1]
	} else {
		tag = &gcpcv1.Tag{}
	}
	tag.Key = append(tag.Key[:0], key...)
	tag.Value = append(tag.Value[:0], value...)
	return tag
}

func (s *drainWorkerScratch) borrowTelemetryItem() *gcpcv1.TelemetryItem {
	var telemetryItem *gcpcv1.TelemetryItem
	if itemCount := len(s.protoItems); itemCount > 0 {
		telemetryItem = s.protoItems[itemCount-1]
		s.protoItems = s.protoItems[:itemCount-1]
	} else {
		telemetryItem = &gcpcv1.TelemetryItem{}
	}
	return telemetryItem
}

func (s *drainWorkerScratch) returnTelemetryItems(items []*gcpcv1.TelemetryItem) {
	for _, telemetryItem := range items {
		telemetryItem.Kind = 0
		telemetryItem.Payload = nil
	}
	s.protoItems = append(s.protoItems, items...)
}

func (s *drainWorkerScratch) returnTags(tags []*gcpcv1.Tag) {
	for _, tag := range tags {
		tag.Key = tag.Key[:0]
		tag.Value = tag.Value[:0]
	}
	s.protoTags = append(s.protoTags, tags...)
}

func newGapJanitor(interval time.Duration) *gapJanitor {
	if interval <= 0 {
		interval = defaultOperationTrackerGapInterval
	}
	return &gapJanitor{
		interval:       interval,
		lastSampleTime: time.Now(),
	}
}

func (j *gapJanitor) sample(manager *commonobs.SlotOperationTrackerManager, now time.Time) *apievents.Event {
	if j == nil || manager == nil {
		return nil
	}
	if j.interval <= 0 {
		j.interval = defaultOperationTrackerGapInterval
	}
	if now.IsZero() {
		now = time.Now()
	}
	if now.Sub(j.lastSampleTime) < j.interval {
		return nil
	}

	skipped := manager.SkippedOperations()
	dropped := manager.DroppedRecords()
	completed := manager.DroppedCompletedOperations()
	invalid := manager.InvalidHandles()

	deltaSkipped := skipped - j.lastSkipped
	deltaDropped := dropped - j.lastDropped
	deltaCompleted := completed - j.lastCompleted
	deltaInvalid := invalid - j.lastInvalid

	j.lastSkipped = skipped
	j.lastDropped = dropped
	j.lastCompleted = completed
	j.lastInvalid = invalid
	j.lastSampleTime = now

	if deltaSkipped == 0 && deltaDropped == 0 && deltaCompleted == 0 && deltaInvalid == 0 {
		return nil
	}

	windowMs := uint64(j.interval.Milliseconds())
	if windowMs == 0 {
		windowMs = uint64(defaultOperationTrackerGapInterval.Milliseconds())
	}
	event := apievents.NewReplayGap(deltaSkipped, deltaDropped, deltaCompleted, deltaInvalid, windowMs)
	return &event
}

// NewOperationTrackerDrainWorker returns a server-side completed-operation
// drain worker. The tracker itself remains the reusable commons mechanism; this
// worker only owns server projection and lifecycle.
func NewOperationTrackerDrainWorker(manager *commonobs.SlotOperationTrackerManager, interval time.Duration) *OperationTrackerDrainWorker {
	if interval <= 0 {
		interval = defaultOperationTrackerDrainInterval
	}
	worker := &OperationTrackerDrainWorker{
		manager:     manager,
		interval:    interval,
		gapInterval: defaultOperationTrackerGapInterval,
		emitter:     apievents.NoopEmitter{},
		stopCh:      make(chan struct{}),
		idleBackoff: 1 * time.Millisecond,
	}
	worker.SetWorkerCount(8)
	if manager != nil {
		manager.SetCompletedNotify(worker.nudge)
	}
	return worker
}

// SetWorkerCount configures how many disjoint shard ranges drain completed
// operations. Calls after Start are ignored to keep worker lifecycle ownership
// stable.
func (w *OperationTrackerDrainWorker) SetWorkerCount(n int) {
	if w == nil {
		return
	}
	if w.started.Load() {
		logger.WarnNoCtx().Int("requested_worker_count", n).Msg("operation tracker drain worker count change ignored after start")
		return
	}
	w.workerCount = clampDrainWorkerCount(n)
	w.rangeWorkers = w.makeShardRangeWorkers(w.workerCount)
}

func clampDrainWorkerCount(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxOperationTrackerDrainWorkerCount {
		return maxOperationTrackerDrainWorkerCount
	}
	return n
}

func (w *OperationTrackerDrainWorker) makeShardRangeWorkers(workerCount int) []shardRangeWorker {
	if w == nil || w.manager == nil {
		return nil
	}
	shardCount := w.manager.ShardCount()
	if shardCount <= 0 {
		return nil
	}
	workerCount = clampDrainWorkerCount(workerCount)
	rangeWorkers := make([]shardRangeWorker, workerCount)
	for workerIndex := range rangeWorkers {
		shardStart := workerIndex * shardCount / workerCount
		shardEnd := (workerIndex + 1) * shardCount / workerCount
		rangeWorkers[workerIndex].shardStart = shardStart
		rangeWorkers[workerIndex].shardEnd = shardEnd
		rangeWorkers[workerIndex].nudgeCh = make(chan struct{}, 1)
	}
	return rangeWorkers
}

func (w *OperationTrackerDrainWorker) nudge(shard int) {
	if w == nil {
		return
	}
	for workerIndex := range w.rangeWorkers {
		rangeWorker := &w.rangeWorkers[workerIndex]
		if shard < rangeWorker.shardStart || shard >= rangeWorker.shardEnd {
			continue
		}
		select {
		case rangeWorker.nudgeCh <- struct{}{}:
		default:
		}
		return
	}
}

// SetEmitter wires runtime event materialization into the drain worker. The
// worker, not command/server goroutines, performs the final event fanout.
func (w *OperationTrackerDrainWorker) SetEmitter(emitter apievents.Emitter) {
	if w == nil {
		return
	}
	if emitter == nil {
		emitter = apievents.NoopEmitter{}
	}
	w.emitter = emitter
}

// SetTmpfsWriter wires the tmpfs telemetry fan-out used by ScopeTelemetry
// subscribers. The writer may be an io.MultiWriter for per-plugin fan-out.
func (w *OperationTrackerDrainWorker) SetTmpfsWriter(writer io.Writer) {
	if w == nil {
		return
	}
	w.tmpfsWriterMu.Lock()
	w.tmpfsWriter = writer
	w.hasTelemetrySubscriber = writer != nil
	w.tmpfsWriterMu.Unlock()
}

// SetGapInterval configures how often the worker samples OperationTracker loss
// counters. Non-positive values reset to the default ~100 ms cadence.
func (w *OperationTrackerDrainWorker) SetGapInterval(interval time.Duration) {
	if w == nil {
		return
	}
	if interval <= 0 {
		interval = defaultOperationTrackerGapInterval
	}
	w.gapInterval = interval
}

// Start begins event-driven draining until the parent context is cancelled or
// Stop is called. The context is used only as a lifecycle cancellation signal.
func (w *OperationTrackerDrainWorker) Start(parentCtx context.Context) {
	if w == nil || w.manager == nil {
		return
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	gapInterval := w.gapInterval
	if gapInterval <= 0 {
		gapInterval = defaultOperationTrackerGapInterval
	}
	w.ensureRangeWorkers()
	w.started.Store(true)
	w.wg.Add(len(w.rangeWorkers) + 1)
	for workerIndex := range w.rangeWorkers {
		rangeWorker := &w.rangeWorkers[workerIndex]
		go func(activeRangeWorker *shardRangeWorker) {
			defer w.wg.Done()
			idleBackoff := w.idleBackoff
			if idleBackoff <= 0 {
				idleBackoff = 1 * time.Millisecond
			}
			for {
				if w.drainRangeUntilEmpty(activeRangeWorker) == 0 {
					select {
					case <-activeRangeWorker.nudgeCh:
					case <-parentCtx.Done():
						w.drainRangeOnce(activeRangeWorker)
						return
					case <-w.stopCh:
						w.drainRangeOnce(activeRangeWorker)
						return
					case <-time.After(idleBackoff):
					}
					if idleBackoff < w.interval {
						idleBackoff *= 2
						if idleBackoff > w.interval {
							idleBackoff = w.interval
						}
					}
				} else {
					idleBackoff = 1 * time.Millisecond
				}
			}
		}(rangeWorker)
	}
	go func() {
		defer w.wg.Done()
		janitor := newGapJanitor(gapInterval)
		ticker := time.NewTicker(janitor.interval)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				if evt := janitor.sample(w.manager, now); evt != nil {
					if w.emitter != nil {
						w.emitter.Emit(*evt)
					}
					// Production-visible signal: log when telemetry loss is detected
					gap := evt.Proto.GetReplayGap()
					logger.WarnNoCtx().
						Uint64("skipped", gap.SkippedOperations).
						Uint64("dropped_records", gap.DroppedRecords).
						Uint64("dropped_completed", gap.DroppedCompleted).
						Uint64("invalid_handles", gap.InvalidHandles).
						Uint64("window_ms", gap.WindowMs).
						Msg("telemetry gap detected: operation tracker loss counters increased")
				}
			case <-parentCtx.Done():
				return
			case <-w.stopCh:
				return
			}
		}
	}()
}

// Stop signals the worker to stop, waits for its goroutines, and performs a final
// drain from inside the worker before it exits. Stop is idempotent.
func (w *OperationTrackerDrainWorker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	for workerIndex := range w.rangeWorkers {
		rangeWorker := &w.rangeWorkers[workerIndex]
		select {
		case rangeWorker.nudgeCh <- struct{}{}:
		default:
		}
	}
	w.wg.Wait()
}

// DrainOnce drains all completed operation shards once and returns the number of
// completed operations processed.
func (w *OperationTrackerDrainWorker) DrainOnce() int {
	if w == nil || w.manager == nil {
		return 0
	}
	w.ensureRangeWorkers()
	return w.drainRangesOnce()
}

func (w *OperationTrackerDrainWorker) ensureRangeWorkers() {
	if w == nil || w.manager == nil || len(w.rangeWorkers) > 0 {
		return
	}
	workerCount := w.workerCount
	if workerCount == 0 {
		workerCount = 1
	}
	w.rangeWorkers = w.makeShardRangeWorkers(workerCount)
}

func (w *OperationTrackerDrainWorker) drainRangesOnce() int {
	if len(w.rangeWorkers) == 0 {
		return 0
	}
	var totalDrained atomic.Uint64
	var drainWaitGroup sync.WaitGroup
	drainWaitGroup.Add(len(w.rangeWorkers))
	for workerIndex := range w.rangeWorkers {
		rangeWorker := &w.rangeWorkers[workerIndex]
		go func(activeRangeWorker *shardRangeWorker) {
			defer drainWaitGroup.Done()
			drained := w.drainRangeOnce(activeRangeWorker)
			if drained > 0 {
				totalDrained.Add(uint64(drained))
			}
		}(rangeWorker)
	}
	drainWaitGroup.Wait()
	return int(totalDrained.Load())
}

func (w *OperationTrackerDrainWorker) drainRangeUntilEmpty(rangeWorker *shardRangeWorker) int {
	if w == nil || w.manager == nil || rangeWorker == nil {
		return 0
	}
	total := 0
	for {
		drained := w.drainRangeOnce(rangeWorker)
		if drained == 0 {
			return total
		}
		total += drained
	}
}

func (w *OperationTrackerDrainWorker) drainRangeOnce(rangeWorker *shardRangeWorker) int {
	if w == nil || w.manager == nil || rangeWorker == nil {
		return 0
	}
	rangeWorker.drainMu.Lock()
	defer rangeWorker.drainMu.Unlock()
	drained := 0
	for shard := rangeWorker.shardStart; shard < rangeWorker.shardEnd; shard++ {
		drained += w.manager.DrainCompletedShard(shard, func(operation commonobs.CompletedOperation) {
			w.projectCompletedOperation(operation, &rangeWorker.scratch)
		})
	}
	return drained
}

func (w *OperationTrackerDrainWorker) projectCompletedOperation(operation commonobs.CompletedOperation, scratch *drainWorkerScratch) {
	if scratch == nil {
		return
	}
	w.tmpfsWriterMu.RLock()
	hasTelemetrySubscriber := w.hasTelemetrySubscriber
	w.tmpfsWriterMu.RUnlock()
	if hasTelemetrySubscriber {
		if scratch.telemetryOp == nil {
			scratch.telemetryOp = &gcpcv1.TelemetryOperation{}
		}
		telemetryOperation := scratch.telemetryOp
		telemetryOperation.OperationId = strconv.FormatInt(int64(operation.Operation), 10)
		telemetryOperation.InitialContext = telemetryOperation.InitialContext[:0]
		telemetryOperation.TelemetryItems = telemetryOperation.TelemetryItems[:0]

		if w.manager != nil && !operation.ContextVersion.IsZero() {
			w.manager.VisitConnectionContextVersion(operation.ContextVersion, func(contextKey, contextValue string) bool {
				if !apictx.IsTelemetryVisible(contextKey) {
					return true
				}
				telemetryOperation.InitialContext = append(telemetryOperation.InitialContext, scratch.borrowTag(contextKey, contextValue))
				return true
			})
		}
		for contextKey, contextValue := range operation.ContextOverlay {
			if !apictx.IsTelemetryVisible(contextKey) {
				continue
			}
			telemetryOperation.InitialContext = append(telemetryOperation.InitialContext, scratch.borrowTag(contextKey, contextValue))
		}

		for recordIndex := range operation.Records {
			record := operation.Records[recordIndex]
			telemetryKind := gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_UNSPECIFIED
			switch record.Kind {
			case apiobs.TelemetryRecordOperationStart:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_START
			case apiobs.TelemetryRecordOperationFinish:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH
			case apiobs.TelemetryRecordCommandStart:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_START
			case apiobs.TelemetryRecordCommandFinish:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_FINISH
			case apiobs.TelemetryRecordContextUpdate:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_UPDATE
			case apiobs.TelemetryRecordContextRemove:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_REMOVE
			case apiobs.TelemetryRecordLog:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_LOG
			case apiobs.TelemetryRecordEvent:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_EVENT
			case apiobs.TelemetryRecordDrop:
				telemetryKind = gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_DROP
			}
			telemetryItem := scratch.borrowTelemetryItem()
			telemetryItem.Kind = telemetryKind
			telemetryItem.Payload = record.PayloadBytes()
			telemetryOperation.TelemetryItems = append(telemetryOperation.TelemetryItems, telemetryItem)
		}
		telemetryOperation.CommandCount = 0
		for _, telemetryItem := range telemetryOperation.TelemetryItems {
			if telemetryItem.Kind == gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_START {
				telemetryOperation.CommandCount++
			}
		}

		operationSize := telemetryOperation.SizeVT()
		if cap(scratch.marshalBuf) < operationSize {
			scratch.marshalBuf = make([]byte, operationSize)
		} else {
			scratch.marshalBuf = scratch.marshalBuf[:operationSize]
		}
		serializedLength, marshalErr := telemetryOperation.MarshalToVT(scratch.marshalBuf)
		if marshalErr != nil {
			logger.WarnNoCtx().Err(marshalErr).Msg("marshal completed operation telemetry")
		} else {
			w.tmpfsWriterMu.RLock()
			telemetryWriter := w.tmpfsWriter
			if telemetryWriter != nil {
				_, writeErr := telemetryWriter.Write(scratch.marshalBuf[:serializedLength])
				if writeErr != nil {
					logger.WarnNoCtx().Err(writeErr).Msg("write completed operation telemetry to tmpfs")
				}
			}
			w.tmpfsWriterMu.RUnlock()
		}

		scratch.returnTelemetryItems(telemetryOperation.TelemetryItems)
		telemetryOperation.TelemetryItems = telemetryOperation.TelemetryItems[:0]
		scratch.returnTags(telemetryOperation.InitialContext)
		telemetryOperation.InitialContext = telemetryOperation.InitialContext[:0]
	}

	hasEventSubscribers := w.emitter != nil && w.emitter.HasSubscribers()
	var operationContext map[string]string
	if hasEventSubscribers {
		operationContext = w.copyOperationContext(operation)
	}
	for i := range operation.Records {
		scratch.reset()
		record := operation.Records[i]
		switch record.Kind {
		case apiobs.TelemetryRecordContextUpdate:
			if hasEventSubscribers {
				operationContext = foldContextUpdate(operationContext, record)
			}
		case apiobs.TelemetryRecordContextRemove:
			if hasEventSubscribers {
				foldContextRemove(operationContext, record)
			}
		case apiobs.TelemetryRecordOperationStart:
			if w.emitter != nil && w.emitter.HasSubscribersFor(apievents.OperationStarted) {
				materializeOperationStartedRecord(operation, record, operationContext, w.emitter, scratch)
			}
		case apiobs.TelemetryRecordOperationFinish:
			if w.emitter != nil && w.emitter.HasSubscribersFor(apievents.OperationCompleted) {
				materializeOperationFinishedRecord(operation, record, operationContext, w.emitter, scratch)
			}
		case apiobs.TelemetryRecordCommandStart:
			if w.emitter != nil && w.emitter.HasSubscribersFor(apievents.CommandStarted) {
				materializeCommandStartedRecord(record, w.emitter, scratch)
			}
		case apiobs.TelemetryRecordCommandFinish:
			if w.emitter != nil && w.emitter.HasSubscribersFor(apievents.CommandCompleted) {
				materializeCommandFinishedRecord(record, w.emitter, scratch)
			}
		case apiobs.TelemetryRecordLog:
			materializeCompletedOperationLog(operation, record, operationContext)
		case apiobs.TelemetryRecordEvent:
			if hasEventSubscribers {
				materializeCompletedOperationEvent(record, operationContext, w.emitter, scratch)
			}
		case apiobs.TelemetryRecordDrop:
			if hasEventSubscribers {
				materializeDropRecord(operation, record, operationContext, scratch)
			}
		}
	}
}

func (w *OperationTrackerDrainWorker) copyOperationContext(operation commonobs.CompletedOperation) map[string]string {
	var operationContext map[string]string
	if w != nil && w.manager != nil && !operation.ContextVersion.IsZero() {
		w.manager.VisitConnectionContextVersion(operation.ContextVersion, func(key, value string) bool {
			if operationContext == nil {
				operationContext = make(map[string]string)
			}
			operationContext[key] = value
			return true
		})
	}
	for key, value := range operation.ContextOverlay {
		if operationContext == nil {
			operationContext = make(map[string]string, len(operation.ContextOverlay))
		}
		operationContext[key] = value
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

func materializeOperationStartedRecord(operation commonobs.CompletedOperation, record apiobs.TelemetryRecord, operationContext map[string]string, emitter apievents.Emitter, scratch *drainWorkerScratch) {
	if emitter == nil {
		return
	}
	fields := scratchTelemetryRecordFields(record, scratch)
	operationID := fieldOrDefault(fields, apicommand.OperationID, strconv.FormatInt(int64(operation.Operation), 10))
	operationType := string(record.NameBytes())
	if operationType == "" {
		operationType = fieldValue(fields, "_operation_type")
	}
	parentID := fieldOrDefault(fields, "_parent_operation_id", operation.Parent.String())
	event := apievents.NewOperationStarted(operationID, operationType, parentID, cloneStringMap(operationContext))
	applyRecordTimestamp(&event, record)
	emitter.Emit(event)
}

func materializeOperationFinishedRecord(operation commonobs.CompletedOperation, record apiobs.TelemetryRecord, operationContext map[string]string, emitter apievents.Emitter, scratch *drainWorkerScratch) {
	if emitter == nil {
		return
	}
	fields := scratchTelemetryRecordFields(record, scratch)
	operationID := fieldOrDefault(fields, apicommand.OperationID, strconv.FormatInt(int64(operation.Operation), 10))
	operationType := string(record.NameBytes())
	if operationType == "" {
		operationType = fieldValue(fields, "_operation_type")
	}
	elapsedNs := uint64(record.Number)
	if elapsedNs == 0 {
		elapsedNs, _ = strconv.ParseUint(fieldValue(fields, apicommand.ElapsedNs), 10, 64)
	}
	status := fieldOrDefault(fields, "_status", completedStatusString(operation.Status))
	event := apievents.NewOperationCompleted(operationID, operationType, elapsedNs, status, fieldValue(fields, apicommand.ErrorKey), cloneStringMap(operationContext))
	applyRecordTimestamp(&event, record)
	emitter.Emit(event)
}

func materializeCommandStartedRecord(record apiobs.TelemetryRecord, emitter apievents.Emitter, scratch *drainWorkerScratch) {
	if emitter == nil {
		return
	}
	fields := scratchTelemetryRecordFields(record, scratch)
	event := apievents.NewCommandStarted(
		string(record.NameBytes()),
		stableEventArgs(scratchEventArgs(fields, scratch), emitter),
		stableEventMetadata(scratchEventMetadata(fields, scratch), emitter),
	)
	if operationID := fieldValue(fields, apicommand.OperationID); operationID != "" {
		event = event.WithOperationID(operationID)
	}
	applyRecordTimestamp(&event, record)
	emitter.Emit(event)
}

func materializeCommandFinishedRecord(record apiobs.TelemetryRecord, emitter apievents.Emitter, scratch *drainWorkerScratch) {
	if emitter == nil {
		return
	}
	fields := scratchTelemetryRecordFields(record, scratch)
	elapsedNs := uint64(record.Number)
	if elapsedNs == 0 {
		elapsedNs, _ = strconv.ParseUint(fieldValue(fields, apicommand.ElapsedNs), 10, 64)
	}
	event := apievents.NewCommandCompleted(
		string(record.NameBytes()),
		stableEventArgs(scratchEventArgs(fields, scratch), emitter),
		elapsedNs,
		fieldValue(fields, apicommand.ResultKey),
		fieldValue(fields, apicommand.ErrorKey),
		stableEventMetadata(scratchEventMetadata(fields, scratch), emitter),
	)
	if operationID := fieldValue(fields, apicommand.OperationID); operationID != "" {
		event = event.WithOperationID(operationID)
	}
	applyRecordTimestamp(&event, record)
	emitter.Emit(event)
}

func materializeCompletedOperationEvent(record apiobs.TelemetryRecord, operationContext map[string]string, emitter apievents.Emitter, scratch *drainWorkerScratch) {
	if emitter == nil {
		return
	}
	fields := scratchTelemetryRecordFields(record, scratch)
	eventType := apievents.Type(string(record.NameBytes()))
	var event apievents.Event
	switch eventType {
	case apievents.ConnectionOpen:
		event = apievents.NewConnectionOpen(fieldValue(fields, apicommand.RemoteAddrKey), fieldValue(fields, apicommand.ConnectionIDKey))
	case apievents.ConnectionClose:
		durationNs, _ := strconv.ParseUint(fieldValue(fields, apicommand.ElapsedNs), 10, 64)
		event = apievents.NewConnectionClose(fieldValue(fields, apicommand.RemoteAddrKey), durationNs, fieldValue(fields, apicommand.ConnectionIDKey))
	case apievents.OperationStarted:
		event = apievents.NewOperationStarted(fieldValue(fields, apicommand.OperationID), fieldValue(fields, "_operation_type"), fieldValue(fields, "_parent_operation_id"), cloneStringMap(operationContext))
	case apievents.OperationCompleted:
		durationNs, _ := strconv.ParseUint(fieldValue(fields, apicommand.ElapsedNs), 10, 64)
		event = apievents.NewOperationCompleted(fieldValue(fields, apicommand.OperationID), fieldValue(fields, "_operation_type"), durationNs, fieldValue(fields, "_status"), fieldValue(fields, apicommand.ErrorKey), cloneStringMap(operationContext))
	case apievents.CommandStarted:
		event = apievents.NewCommandStarted(fieldValue(fields, apicommand.CommandKey), stableEventArgs(scratchEventArgs(fields, scratch), emitter), stableEventMetadata(scratchEventMetadata(fields, scratch), emitter))
	case apievents.CommandCompleted:
		durationNs, _ := strconv.ParseUint(fieldValue(fields, apicommand.ElapsedNs), 10, 64)
		event = apievents.NewCommandCompleted(fieldValue(fields, apicommand.CommandKey), stableEventArgs(scratchEventArgs(fields, scratch), emitter), durationNs, fieldValue(fields, apicommand.ResultKey), fieldValue(fields, apicommand.ErrorKey), stableEventMetadata(scratchEventMetadata(fields, scratch), emitter))
	case apievents.AuthFailed:
		event = apievents.NewAuthFailed(fieldValue(fields, apicommand.RemoteAddrKey), fieldValue(fields, apicommand.CommandKey))
	case apievents.ServerShutdown:
		event = apievents.NewServerShutdown(fieldValue(fields, "_reason"))
	case apievents.PluginRegistered:
		critical, _ := strconv.ParseBool(fieldValue(fields, "_critical"))
		event = apievents.NewPluginRegistered(fieldValue(fields, apicommand.PluginNameKey), fieldValue(fields, "_version"), critical)
	case apievents.PluginCrashed:
		critical, _ := strconv.ParseBool(fieldValue(fields, "_critical"))
		event = apievents.NewPluginCrashed(fieldValue(fields, apicommand.PluginNameKey), critical, fieldValue(fields, apicommand.ErrorKey))
	case apievents.PluginRestarted:
		critical, _ := strconv.ParseBool(fieldValue(fields, "_critical"))
		restartCount, _ := strconv.Atoi(fieldValue(fields, "_restart_count"))
		event = apievents.NewPluginRestarted(fieldValue(fields, apicommand.PluginNameKey), critical, restartCount)
	case apievents.PluginStarted:
		critical, _ := strconv.ParseBool(fieldValue(fields, "_critical"))
		pid, _ := strconv.Atoi(fieldValue(fields, "_pid"))
		event = apievents.NewPluginStarted(fieldValue(fields, apicommand.PluginNameKey), critical, pid)
	case apievents.PluginStopped:
		critical, _ := strconv.ParseBool(fieldValue(fields, "_critical"))
		event = apievents.NewPluginStopped(fieldValue(fields, apicommand.PluginNameKey), critical, fieldValue(fields, "_reason"))
	case apievents.PluginRegistrationFailed:
		critical, _ := strconv.ParseBool(fieldValue(fields, "_critical"))
		event = apievents.NewPluginRegistrationFailed(fieldValue(fields, apicommand.PluginNameKey), fieldValue(fields, "_version"), critical, fieldValue(fields, apicommand.ErrorKey))
	case apievents.PluginCommandRegistered:
		namespaced, _ := strconv.ParseBool(fieldValue(fields, "_namespaced"))
		readonly, _ := strconv.ParseBool(fieldValue(fields, "_readonly"))
		event = apievents.NewPluginCommandRegistered(fieldValue(fields, apicommand.PluginNameKey), fieldValue(fields, apicommand.CommandKey), namespaced, readonly)
	case apievents.PluginCommandRegistrationFailed:
		event = apievents.NewPluginCommandRegistrationFailed(fieldValue(fields, apicommand.PluginNameKey), fieldValue(fields, apicommand.CommandKey), fieldValue(fields, apicommand.ErrorKey))
	case apievents.ConfigReloaded:
		event = apievents.NewConfigReloaded(fieldValue(fields, apicommand.FileKey))
	case apievents.CacheEviction:
		event = apievents.NewCacheEviction(fieldValue(fields, "_key"), fieldValue(fields, "_reason"))
	default:
		return
	}
	if operationID := fieldValue(fields, apicommand.OperationID); operationID != "" {
		event = event.WithOperationID(operationID)
	}
	if record.TimestampUnixNano > 0 {
		event.Proto.Timestamp = uint64(record.TimestampUnixNano)
	}
	emitter.Emit(event)
}

func decodeTelemetryRecordFields(record apiobs.TelemetryRecord, scratch []kvPair) []kvPair {
	scratch = scratch[:0]
	payload := record.PayloadBytes()
	if len(payload) == 0 {
		return scratch
	}
	pos := 1
	for count := int(payload[0]); count > 0; count-- {
		if pos >= len(payload) {
			return scratch
		}
		keyLen := int(payload[pos])
		pos++
		if pos+keyLen > len(payload) {
			return scratch
		}
		key := string(payload[pos : pos+keyLen])
		pos += keyLen
		if pos >= len(payload) {
			return scratch
		}
		valueLen := int(payload[pos])
		pos++
		if pos+valueLen > len(payload) {
			return scratch
		}
		value := string(payload[pos : pos+valueLen])
		pos += valueLen
		scratch = append(scratch, kvPair{key: key, value: value})
	}
	return scratch
}

func scratchTelemetryRecordFields(record apiobs.TelemetryRecord, scratch *drainWorkerScratch) []kvPair {
	if scratch == nil {
		return decodeTelemetryRecordFields(record, nil)
	}
	scratch.fields = decodeTelemetryRecordFields(record, scratch.fields)
	return scratch.fields
}

func fieldFromPairs(pairs []kvPair, key string) (string, bool) {
	for i := len(pairs) - 1; i >= 0; i-- {
		if pairs[i].key == key {
			return pairs[i].value, true
		}
	}
	return "", false
}

func fieldValue(pairs []kvPair, key string) string {
	value, _ := fieldFromPairs(pairs, key)
	return value
}

func decodeEventArgs(pairs []kvPair, scratch []string) []string {
	countValue, _ := fieldFromPairs(pairs, "_args_count")
	count, _ := strconv.Atoi(countValue)
	if count <= 0 {
		return scratch[:0]
	}
	if cap(scratch) < count {
		scratch = make([]string, count)
	} else {
		scratch = scratch[:count]
	}
	for i := range scratch {
		scratch[i] = ""
	}
	const prefix = "_arg."
	for _, pair := range pairs {
		if len(pair.key) <= len(prefix) || pair.key[:len(prefix)] != prefix {
			continue
		}
		idx, ok := parseCanonicalArgIndex(pair.key[len(prefix):])
		if ok && idx < count {
			scratch[idx] = pair.value
		}
	}
	return scratch
}

func scratchEventArgs(pairs []kvPair, scratch *drainWorkerScratch) []string {
	if scratch == nil {
		return decodeEventArgs(pairs, nil)
	}
	scratch.args = decodeEventArgs(pairs, scratch.args)
	return scratch.args
}

func parseCanonicalArgIndex(value string) (int, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	idx := 0
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch < '0' || ch > '9' {
			return 0, false
		}
		idx = idx*10 + int(ch-'0')
		if idx < 0 {
			return 0, false
		}
	}
	return idx, true
}

func decodeEventMetadata(pairs []kvPair, scratch []kvPair) []kvPair {
	scratch = scratch[:0]
	const prefix = "_metadata."
	for _, pair := range pairs {
		if len(pair.key) > len(prefix) && pair.key[:len(prefix)] == prefix {
			scratch = append(scratch, kvPair{key: pair.key[len(prefix):], value: pair.value})
		}
	}
	return scratch
}

func scratchEventMetadata(pairs []kvPair, scratch *drainWorkerScratch) []kvPair {
	if scratch == nil {
		return decodeEventMetadata(pairs, nil)
	}
	scratch.metadata = decodeEventMetadata(pairs, scratch.metadata)
	return scratch.metadata
}

func stableEventArgs(args []string, emitter apievents.Emitter) []string {
	if len(args) == 0 {
		return nil
	}
	if eventPayloadCanUseScratch(emitter) {
		return args
	}
	stable := make([]string, len(args))
	copy(stable, args)
	return stable
}

func stableEventMetadata(pairs []kvPair, emitter apievents.Emitter) map[string]string {
	if len(pairs) == 0 || eventPayloadCanUseScratch(emitter) {
		return nil
	}
	metadata := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		metadata[pair.key] = pair.value
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

func eventPayloadCanUseScratch(emitter apievents.Emitter) bool {
	if emitter == nil {
		return true
	}
	switch emitter.(type) {
	case apievents.NoopEmitter, *apievents.NoopEmitter:
		return true
	default:
		return false
	}
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
func fieldOrDefault(fields []kvPair, key, fallback string) string {
	if value, ok := fieldFromPairs(fields, key); ok && value != "" {
		return value
	}
	return fallback
}

func completedStatusString(status commonobs.SlotTerminalStatus) string {
	switch status {
	case commonobs.SlotTerminalFinished:
		return "completed"
	case commonobs.SlotTerminalFailed:
		return "failed"
	case commonobs.SlotTerminalTimedOut:
		return "timed_out"
	case commonobs.SlotTerminalAbandoned:
		return "abandoned"
	default:
		return "unknown"
	}
}

func applyRecordTimestamp(event *apievents.Event, record apiobs.TelemetryRecord) {
	if event == nil || event.Proto == nil || record.TimestampUnixNano == 0 {
		return
	}
	event.Proto.Timestamp = uint64(record.TimestampUnixNano)
}

func materializeDropRecord(operation commonobs.CompletedOperation, record apiobs.TelemetryRecord, operationContext map[string]string, scratch *drainWorkerScratch) {
	event := logger.TelemetryNoCtx(apiobs.TelemetryLogLevelWarn)
	if event == nil {
		return
	}
	if !operation.Parent.IsZero() {
		event = event.Str(operationTrackerDrainParentField, operation.Parent.String())
	}
	if len(operationContext) > 0 {
		redactedContext := apictx.RedactSecrets(operationContext)
		if len(redactedContext) > 0 {
			event = event.Interface(apicommand.CtxField, redactedContext)
		}
	}
	fields := scratchTelemetryRecordFields(record, scratch)
	for i, pair := range fields {
		if hasLaterPairWithKey(fields, i, pair.key) {
			continue
		}
		event = event.Str(pair.key, pair.value)
	}
	event.Msg(string(record.NameBytes()))
}

func hasLaterPairWithKey(pairs []kvPair, index int, key string) bool {
	for i := index + 1; i < len(pairs); i++ {
		if pairs[i].key == key {
			return true
		}
	}
	return false
}

func materializeCompletedOperationLog(operation commonobs.CompletedOperation, record apiobs.TelemetryRecord, operationContext map[string]string) {
	if record.Flags&apiobs.TelemetryRecordFlagLocalLogMaterialized != 0 {
		return
	}
	event := logger.TelemetryNoCtx(record.Level)
	if event == nil {
		return
	}
	if !operation.Parent.IsZero() {
		event = event.Str(operationTrackerDrainParentField, operation.Parent.String())
	}
	if len(operationContext) > 0 {
		redactedContext := apictx.RedactSecrets(operationContext)
		if len(redactedContext) > 0 {
			event = event.Interface(apicommand.CtxField, redactedContext)
		}
	}
	for i := 0; i < int(record.FieldCount); i++ {
		key, value, ok := record.FieldBytes(i)
		if !ok {
			break
		}
		event = event.Str(string(key), string(value))
	}
	if record.DroppedFields > 0 {
		event = event.Int("_dropped_fields", int(record.DroppedFields))
	}
	if operation.DroppedRecords > 0 {
		event = event.Uint64("_dropped_records", operation.DroppedRecords)
	}
	event.Msg(string(record.NameBytes()))
}
