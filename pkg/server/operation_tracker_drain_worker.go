package server

import (
	"context"
	"strconv"
	"sync"
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

const operationTrackerDrainParentField = "_parent_operation_id"

// OperationTrackerDrainWorker owns server-side projection of completed
// OperationTracker slots. It drains and materializes accepted telemetry records
// after command execution has finished, keeping zerolog formatting and sink I/O
// off the command goroutine.
type OperationTrackerDrainWorker struct {
	manager     *commonobs.SlotOperationTrackerManager
	interval    time.Duration
	gapInterval time.Duration
	emitter     apievents.Emitter
	nudgeCh     chan struct{} // buffered 1, non-blocking nudge
	idleBackoff time.Duration // exponential backoff for idle state
	drainMu     sync.Mutex
	scratch     drainWorkerScratch

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type drainWorkerScratch struct {
	fields     []kvPair
	args       []string
	metadata   []kvPair
	eventBuf   *gcpcv1.EventV1
	contextMap map[string]string
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
		nudgeCh:     make(chan struct{}, 1),
		idleBackoff: 1 * time.Millisecond,
	}
	if manager != nil {
		manager.SetCompletedNotify(worker.nudge)
	}
	return worker
}

func (w *OperationTrackerDrainWorker) nudge() {
	if w == nil {
		return
	}
	select {
	case w.nudgeCh <- struct{}{}:
	default:
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
	w.wg.Add(2)
	go func() {
		defer w.wg.Done()
		w.DrainOnce()
		for {
			if w.drainUntilEmpty() == 0 {
				select {
				case <-w.nudgeCh:
				case <-parentCtx.Done():
					w.DrainOnce()
					return
				case <-w.stopCh:
					w.DrainOnce()
					return
				case <-time.After(w.idleBackoff):
				}
				if w.idleBackoff < w.interval {
					w.idleBackoff *= 2
					if w.idleBackoff > w.interval {
						w.idleBackoff = w.interval
					}
				}
			} else {
				w.idleBackoff = 1 * time.Millisecond
			}
		}
	}()
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
	w.nudge()
	w.wg.Wait()
}

// DrainOnce drains all completed operation shards once and returns the number of
// completed operations processed.
func (w *OperationTrackerDrainWorker) DrainOnce() int {
	if w == nil || w.manager == nil {
		return 0
	}
	w.drainMu.Lock()
	defer w.drainMu.Unlock()
	drained := 0
	for shard := 0; shard < w.manager.ShardCount(); shard++ {
		drained += w.manager.DrainCompletedShard(shard, w.projectCompletedOperation)
	}
	return drained
}

func (w *OperationTrackerDrainWorker) drainUntilEmpty() int {
	if w == nil || w.manager == nil {
		return 0
	}
	w.drainMu.Lock()
	defer w.drainMu.Unlock()
	total := 0
	for {
		drained := 0
		for shard := 0; shard < w.manager.ShardCount(); shard++ {
			drained += w.manager.DrainCompletedShard(shard, w.projectCompletedOperation)
		}
		if drained == 0 {
			return total
		}
		total += drained
	}
}

func (w *OperationTrackerDrainWorker) projectCompletedOperation(operation commonobs.CompletedOperation) {
	scratch := &w.scratch
	operationContext := w.copyOperationContext(operation)
	for i := range operation.Records {
		scratch.reset()
		record := operation.Records[i]
		switch record.Kind {
		case apiobs.TelemetryRecordContextUpdate:
			operationContext = foldContextUpdate(operationContext, record)
		case apiobs.TelemetryRecordContextRemove:
			foldContextRemove(operationContext, record)
		case apiobs.TelemetryRecordOperationStart:
			materializeOperationStartedRecord(operation, record, operationContext, w.emitter, scratch)
		case apiobs.TelemetryRecordOperationFinish:
			materializeOperationFinishedRecord(operation, record, operationContext, w.emitter, scratch)
		case apiobs.TelemetryRecordCommandStart:
			materializeCommandStartedRecord(record, w.emitter, scratch)
		case apiobs.TelemetryRecordCommandFinish:
			materializeCommandFinishedRecord(record, w.emitter, scratch)
		case apiobs.TelemetryRecordLog:
			materializeCompletedOperationLog(operation, record, operationContext)
		case apiobs.TelemetryRecordEvent:
			materializeCompletedOperationEvent(record, operationContext, w.emitter, scratch)
		case apiobs.TelemetryRecordDrop:
			materializeDropRecord(operation, record, operationContext, scratch)
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
