package observability

import apiobs "gocache/api/observability"

// OperationScope bundles the reusable tracker manager with the internal slot
// handle and public operation reference for one active operation. It is a small
// value used to keep function signatures readable while avoiding context.Context
// as a telemetry carrier.
type OperationScope struct {
	manager   *SlotOperationTrackerManager
	handle    InternalTrackerHandle
	operation apiobs.InternalOperationIdentity
	ref       apiobs.OperationRef
}

// NewOperationScope returns a telemetry submission scope for an active slot.
func NewOperationScope(manager *SlotOperationTrackerManager, handle InternalTrackerHandle, operation apiobs.InternalOperationIdentity, ref apiobs.OperationRef) OperationScope {
	return OperationScope{manager: manager, handle: handle, operation: operation, ref: ref}
}

// IsZero reports whether the scope cannot submit telemetry.
func (s OperationScope) IsZero() bool {
	return s.manager == nil || s.handle.IsZero() || s.operation.IsZero()
}

// Operation returns the internal operation identity used for tracker routing.
func (s OperationScope) Operation() apiobs.InternalOperationIdentity {
	return s.operation
}

// Ref returns the public operation reference visible at API/plugin/log/event
// boundaries.
func (s OperationScope) Ref() apiobs.OperationRef {
	return s.ref
}

// Record submits a compact telemetry record for this operation. The scope owns
// the operation identity, so the submitted copy is always rewritten to the
// scope's internal operation id before entering the tracker.
func (s OperationScope) Record(record apiobs.TelemetryRecord) bool {
	if s.IsZero() {
		return false
	}
	record.Operation = s.operation
	return s.manager.RecordTelemetry(s.handle, record)
}

// Log submits a compact log-request record. It does not materialize zerolog or
// any other sink on the caller goroutine.
func (s OperationScope) Log(level apiobs.TelemetryLogLevel, message []byte) bool {
	if s.IsZero() {
		return false
	}
	record := apiobs.NewLogRecordBytes(s.operation, level, message)
	record.TimestampUnixNano = nowUnixNano()
	return s.Record(record)
}

// LogString submits a compact log-request record from a string-backed message
// without forcing the caller to allocate a temporary []byte.
func (s OperationScope) LogString(level apiobs.TelemetryLogLevel, message string) bool {
	if s.IsZero() {
		return false
	}
	record := apiobs.NewLogRecordString(s.operation, level, message)
	record.TimestampUnixNano = nowUnixNano()
	return s.Record(record)
}

// OperationStartString submits a compact operation-start materialization record.
func (s OperationScope) OperationStartString(opType string, fields ...string) bool {
	return s.recordNamedString(apiobs.TelemetryRecordOperationStart, opType, 0, fields...)
}

// OperationFinishString submits a compact operation-finish materialization record.
func (s OperationScope) OperationFinishString(opType string, elapsedNs uint64, fields ...string) bool {
	return s.recordNamedString(apiobs.TelemetryRecordOperationFinish, opType, int64(elapsedNs), fields...)
}

// CommandStartString submits a compact command-start materialization record.
func (s OperationScope) CommandStartString(command string, fields ...string) bool {
	return s.recordNamedString(apiobs.TelemetryRecordCommandStart, command, 0, fields...)
}

// CommandFinishString submits a compact command-finish materialization record.
func (s OperationScope) CommandFinishString(command string, elapsedNs uint64, fields ...string) bool {
	return s.recordNamedString(apiobs.TelemetryRecordCommandFinish, command, int64(elapsedNs), fields...)
}

// EventString submits a compact event-request record from string-backed names
// and fields without materializing the final event on the caller goroutine.
func (s OperationScope) EventString(eventName string, fields ...string) bool {
	return s.recordNamedString(apiobs.TelemetryRecordEvent, eventName, 0, fields...)
}

// DropString submits a compact drop diagnostic record for worker-side
// materialization.
func (s OperationScope) DropString(reason string, fields ...string) bool {
	return s.recordNamedString(apiobs.TelemetryRecordDrop, reason, 0, fields...)
}

func (s OperationScope) recordNamedString(kind apiobs.TelemetryRecordKind, name string, number int64, fields ...string) bool {
	if s.IsZero() {
		return false
	}
	record := apiobs.NewTelemetryRecord(kind, s.operation)
	record.TimestampUnixNano = nowUnixNano()
	record.Number = number
	record.SetNameString(name)
	record.PayloadLen = uint16(packKeyValueStrings(record.Payload[:], fields))
	return s.Record(record)
}

// ContextUpdate submits operation-scoped context key/value deltas for worker-side
// replay. The active operation still only carries its start-time base context
// version; these records are folded over a copied context by drain processors.
func (s OperationScope) ContextUpdate(pairs ...[]byte) bool {
	if s.IsZero() {
		return false
	}
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordContextUpdate, s.operation)
	record.TimestampUnixNano = nowUnixNano()
	record.PayloadLen = uint16(packKeyValues(record.Payload[:], pairs))
	return s.Record(record)
}

// ContextUpdateStrings submits operation-scoped string key/value deltas without
// forcing callers with string-backed metadata to allocate temporary byte slices.
func (s OperationScope) ContextUpdateStrings(pairs ...string) bool {
	if s.IsZero() {
		return false
	}
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordContextUpdate, s.operation)
	record.TimestampUnixNano = nowUnixNano()
	record.PayloadLen = uint16(packKeyValueStrings(record.Payload[:], pairs))
	return s.Record(record)
}

// ContextRemove submits operation-scoped context removals for worker-side replay.
func (s OperationScope) ContextRemove(keys ...[]byte) bool {
	if s.IsZero() {
		return false
	}
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordContextRemove, s.operation)
	record.TimestampUnixNano = nowUnixNano()
	record.PayloadLen = uint16(packKeys(record.Payload[:], keys))
	return s.Record(record)
}

// ContextRemoveStrings submits operation-scoped string removals without forcing
// callers with string-backed metadata to allocate temporary byte slices.
func (s OperationScope) ContextRemoveStrings(keys ...string) bool {
	if s.IsZero() {
		return false
	}
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordContextRemove, s.operation)
	record.TimestampUnixNano = nowUnixNano()
	record.PayloadLen = uint16(packKeyStrings(record.Payload[:], keys))
	return s.Record(record)
}

// ContextSnapshot materializes the active operation context for API/plugin/event
// boundaries. It allocates on purpose and must not be called from the no-fanout
// command telemetry submit path.
func (s OperationScope) ContextSnapshot() map[string]string {
	if s.IsZero() {
		return nil
	}
	return s.manager.OperationContextSnapshot(s.handle)
}

// Finish marks the operation terminal with the supplied status.
func (s OperationScope) Finish(status SlotTerminalStatus) bool {
	if s.IsZero() {
		return false
	}
	return s.manager.FinishOperation(s.handle, status)
}
