package server

import (
	"context"
	"strconv"
	"sync"
	"time"

	apicommand "gocache/api/command"
	apictx "gocache/api/context"
	apievents "gocache/api/events"
	apiobs "gocache/api/observability"
	"gocache/commons/logger"
	commonobs "gocache/commons/observability"
)

const defaultOperationTrackerDrainInterval = 10 * time.Millisecond

const operationTrackerDrainParentField = "_parent_operation_id"

// OperationTrackerDrainWorker owns server-side projection of completed
// OperationTracker slots. It drains and materializes accepted telemetry records
// after command execution has finished, keeping zerolog formatting and sink I/O
// off the command goroutine.
type OperationTrackerDrainWorker struct {
	manager  *commonobs.SlotOperationTrackerManager
	interval time.Duration
	emitter  apievents.Emitter

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewOperationTrackerDrainWorker returns a server-side completed-operation
// drain worker. The tracker itself remains the reusable commons mechanism; this
// worker only owns server projection and lifecycle.
func NewOperationTrackerDrainWorker(manager *commonobs.SlotOperationTrackerManager, interval time.Duration) *OperationTrackerDrainWorker {
	if interval <= 0 {
		interval = defaultOperationTrackerDrainInterval
	}
	return &OperationTrackerDrainWorker{
		manager:  manager,
		interval: interval,
		emitter:  apievents.NoopEmitter{},
		stopCh:   make(chan struct{}),
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

// Start begins periodic draining until the parent context is cancelled or Stop
// is called. The context is used only as a lifecycle cancellation signal.
func (w *OperationTrackerDrainWorker) Start(parentCtx context.Context) {
	if w == nil || w.manager == nil {
		return
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ticker := time.NewTicker(w.interval)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer ticker.Stop()

		w.DrainOnce()
		for {
			select {
			case <-ticker.C:
				w.DrainOnce()
			case <-parentCtx.Done():
				w.DrainOnce()
				return
			case <-w.stopCh:
				w.DrainOnce()
				return
			}
		}
	}()
}

// Stop signals the worker to stop, waits for its goroutine, and performs a final
// drain from inside the worker before it exits. Stop is idempotent.
func (w *OperationTrackerDrainWorker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.wg.Wait()
}

// DrainOnce drains all completed operation shards once and returns the number of
// completed operations processed.
func (w *OperationTrackerDrainWorker) DrainOnce() int {
	if w == nil || w.manager == nil {
		return 0
	}
	drained := 0
	for shard := 0; shard < w.manager.ShardCount(); shard++ {
		drained += w.manager.DrainCompletedShard(shard, w.projectCompletedOperation)
	}
	return drained
}

func (w *OperationTrackerDrainWorker) projectCompletedOperation(operation commonobs.CompletedOperation) {
	operationContext := w.copyOperationContext(operation)
	for i := range operation.Records {
		record := operation.Records[i]
		switch record.Kind {
		case apiobs.TelemetryRecordContextUpdate:
			operationContext = foldContextUpdate(operationContext, record)
		case apiobs.TelemetryRecordContextRemove:
			foldContextRemove(operationContext, record)
		case apiobs.TelemetryRecordOperationStart:
			materializeOperationStartedRecord(operation, record, operationContext, w.emitter)
		case apiobs.TelemetryRecordOperationFinish:
			materializeOperationFinishedRecord(operation, record, operationContext, w.emitter)
		case apiobs.TelemetryRecordCommandStart:
			materializeCommandStartedRecord(record, w.emitter)
		case apiobs.TelemetryRecordCommandFinish:
			materializeCommandFinishedRecord(record, w.emitter)
		case apiobs.TelemetryRecordLog:
			materializeCompletedOperationLog(operation, record, operationContext)
		case apiobs.TelemetryRecordEvent:
			materializeCompletedOperationEvent(record, operationContext, w.emitter)
		case apiobs.TelemetryRecordDrop:
			materializeDropRecord(operation, record, operationContext)
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

func materializeOperationStartedRecord(operation commonobs.CompletedOperation, record apiobs.TelemetryRecord, operationContext map[string]string, emitter apievents.Emitter) {
	if emitter == nil {
		return
	}
	fields := telemetryRecordStringFields(record)
	operationID := fieldOrDefault(fields, apicommand.OperationID, strconv.FormatInt(int64(operation.Operation), 10))
	operationType := string(record.NameBytes())
	if operationType == "" {
		operationType = fields["_operation_type"]
	}
	parentID := fieldOrDefault(fields, "_parent_operation_id", operation.Parent.String())
	event := apievents.NewOperationStarted(operationID, operationType, parentID, cloneStringMap(operationContext))
	applyRecordTimestamp(&event, record)
	emitter.Emit(event)
}

func materializeOperationFinishedRecord(operation commonobs.CompletedOperation, record apiobs.TelemetryRecord, operationContext map[string]string, emitter apievents.Emitter) {
	if emitter == nil {
		return
	}
	fields := telemetryRecordStringFields(record)
	operationID := fieldOrDefault(fields, apicommand.OperationID, strconv.FormatInt(int64(operation.Operation), 10))
	operationType := string(record.NameBytes())
	if operationType == "" {
		operationType = fields["_operation_type"]
	}
	elapsedNs := uint64(record.Number)
	if elapsedNs == 0 {
		elapsedNs, _ = strconv.ParseUint(fields[apicommand.ElapsedNs], 10, 64)
	}
	status := fieldOrDefault(fields, "_status", completedStatusString(operation.Status))
	event := apievents.NewOperationCompleted(operationID, operationType, elapsedNs, status, fields[apicommand.ErrorKey], cloneStringMap(operationContext))
	applyRecordTimestamp(&event, record)
	emitter.Emit(event)
}

func materializeCommandStartedRecord(record apiobs.TelemetryRecord, emitter apievents.Emitter) {
	if emitter == nil {
		return
	}
	fields := telemetryRecordStringFields(record)
	event := apievents.NewCommandStarted(string(record.NameBytes()), eventArgs(fields), eventMetadata(fields))
	if operationID := fields[apicommand.OperationID]; operationID != "" {
		event = event.WithOperationID(operationID)
	}
	applyRecordTimestamp(&event, record)
	emitter.Emit(event)
}

func materializeCommandFinishedRecord(record apiobs.TelemetryRecord, emitter apievents.Emitter) {
	if emitter == nil {
		return
	}
	fields := telemetryRecordStringFields(record)
	elapsedNs := uint64(record.Number)
	if elapsedNs == 0 {
		elapsedNs, _ = strconv.ParseUint(fields[apicommand.ElapsedNs], 10, 64)
	}
	event := apievents.NewCommandCompleted(string(record.NameBytes()), eventArgs(fields), elapsedNs, fields[apicommand.ResultKey], fields[apicommand.ErrorKey], eventMetadata(fields))
	if operationID := fields[apicommand.OperationID]; operationID != "" {
		event = event.WithOperationID(operationID)
	}
	applyRecordTimestamp(&event, record)
	emitter.Emit(event)
}

func materializeCompletedOperationEvent(record apiobs.TelemetryRecord, operationContext map[string]string, emitter apievents.Emitter) {
	if emitter == nil {
		return
	}
	fields := telemetryRecordStringFields(record)
	eventType := apievents.Type(string(record.NameBytes()))
	var event apievents.Event
	switch eventType {
	case apievents.ConnectionOpen:
		event = apievents.NewConnectionOpen(fields[apicommand.RemoteAddrKey], fields[apicommand.ConnectionIDKey])
	case apievents.ConnectionClose:
		durationNs, _ := strconv.ParseUint(fields[apicommand.ElapsedNs], 10, 64)
		event = apievents.NewConnectionClose(fields[apicommand.RemoteAddrKey], durationNs, fields[apicommand.ConnectionIDKey])
	case apievents.OperationStarted:
		event = apievents.NewOperationStarted(fields[apicommand.OperationID], fields["_operation_type"], fields["_parent_operation_id"], cloneStringMap(operationContext))
	case apievents.OperationCompleted:
		durationNs, _ := strconv.ParseUint(fields[apicommand.ElapsedNs], 10, 64)
		event = apievents.NewOperationCompleted(fields[apicommand.OperationID], fields["_operation_type"], durationNs, fields["_status"], fields[apicommand.ErrorKey], cloneStringMap(operationContext))
	case apievents.CommandStarted:
		event = apievents.NewCommandStarted(fields[apicommand.CommandKey], eventArgs(fields), eventMetadata(fields))
	case apievents.CommandCompleted:
		durationNs, _ := strconv.ParseUint(fields[apicommand.ElapsedNs], 10, 64)
		event = apievents.NewCommandCompleted(fields[apicommand.CommandKey], eventArgs(fields), durationNs, fields[apicommand.ResultKey], fields[apicommand.ErrorKey], eventMetadata(fields))
	case apievents.AuthFailed:
		event = apievents.NewAuthFailed(fields[apicommand.RemoteAddrKey], fields[apicommand.CommandKey])
	case apievents.ServerShutdown:
		event = apievents.NewServerShutdown(fields["_reason"])
	case apievents.PluginRegistered:
		critical, _ := strconv.ParseBool(fields["_critical"])
		event = apievents.NewPluginRegistered(fields[apicommand.PluginNameKey], fields["_version"], critical)
	case apievents.PluginCrashed:
		critical, _ := strconv.ParseBool(fields["_critical"])
		event = apievents.NewPluginCrashed(fields[apicommand.PluginNameKey], critical, fields[apicommand.ErrorKey])
	case apievents.PluginRestarted:
		critical, _ := strconv.ParseBool(fields["_critical"])
		restartCount, _ := strconv.Atoi(fields["_restart_count"])
		event = apievents.NewPluginRestarted(fields[apicommand.PluginNameKey], critical, restartCount)
	case apievents.PluginStarted:
		critical, _ := strconv.ParseBool(fields["_critical"])
		pid, _ := strconv.Atoi(fields["_pid"])
		event = apievents.NewPluginStarted(fields[apicommand.PluginNameKey], critical, pid)
	case apievents.PluginStopped:
		critical, _ := strconv.ParseBool(fields["_critical"])
		event = apievents.NewPluginStopped(fields[apicommand.PluginNameKey], critical, fields["_reason"])
	case apievents.PluginRegistrationFailed:
		critical, _ := strconv.ParseBool(fields["_critical"])
		event = apievents.NewPluginRegistrationFailed(fields[apicommand.PluginNameKey], fields["_version"], critical, fields[apicommand.ErrorKey])
	case apievents.PluginCommandRegistered:
		namespaced, _ := strconv.ParseBool(fields["_namespaced"])
		readonly, _ := strconv.ParseBool(fields["_readonly"])
		event = apievents.NewPluginCommandRegistered(fields[apicommand.PluginNameKey], fields[apicommand.CommandKey], namespaced, readonly)
	case apievents.PluginCommandRegistrationFailed:
		event = apievents.NewPluginCommandRegistrationFailed(fields[apicommand.PluginNameKey], fields[apicommand.CommandKey], fields[apicommand.ErrorKey])
	case apievents.ConfigReloaded:
		event = apievents.NewConfigReloaded(fields[apicommand.FileKey])
	case apievents.CacheEviction:
		event = apievents.NewCacheEviction(fields["_key"], fields["_reason"])
	default:
		return
	}
	if operationID := fields[apicommand.OperationID]; operationID != "" {
		event = event.WithOperationID(operationID)
	}
	if record.TimestampUnixNano > 0 {
		event.Proto.Timestamp = uint64(record.TimestampUnixNano)
	}
	emitter.Emit(event)
}

func telemetryRecordStringFields(record apiobs.TelemetryRecord) map[string]string {
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

func eventArgs(fields map[string]string) []string {
	count, _ := strconv.Atoi(fields["_args_count"])
	if count <= 0 {
		return nil
	}
	args := make([]string, count)
	for i := 0; i < count; i++ {
		args[i] = fields["_arg."+strconv.Itoa(i)]
	}
	return args
}

func eventMetadata(fields map[string]string) map[string]string {
	metadata := make(map[string]string)
	for key, value := range fields {
		if len(key) > len("_metadata.") && key[:len("_metadata.")] == "_metadata." {
			metadata[key[len("_metadata."):]] = value
		}
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
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

func fieldOrDefault(fields map[string]string, key, fallback string) string {
	if fields == nil {
		return fallback
	}
	if value := fields[key]; value != "" {
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

func materializeDropRecord(operation commonobs.CompletedOperation, record apiobs.TelemetryRecord, operationContext map[string]string) {
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
	for key, value := range telemetryRecordStringFields(record) {
		event = event.Str(key, value)
	}
	event.Msg(string(record.NameBytes()))
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
