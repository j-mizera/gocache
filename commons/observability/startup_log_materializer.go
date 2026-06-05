package observability

import (
	apicommand "gocache/api/command"
	apiobs "gocache/api/observability"
	"gocache/commons/logger"
)

const parentOperationIDField = "_parent_operation_id"

// StartupLogMaterializer records startup log requests through OperationTracker and
// immediately writes accepted records to the local logger before the server is
// ready. It is intentionally limited to bootstrap/control-plane use; steady-state
// command, connection, cache, and plugin paths must use background materializers.
type StartupLogMaterializer struct{}

// LogBytes creates and records a bytes-backed startup log request.
func (m StartupLogMaterializer) LogBytes(scope OperationScope, level apiobs.TelemetryLogLevel, message []byte) bool {
	record := apiobs.NewLogRecordBytes(scope.Operation(), level, message)
	return m.LogRecord(scope, record)
}

// LogString creates and records a string-backed startup log request. It is for
// startup boundaries that already hold strings; hot paths should use LogBytes or
// build records with byte-backed fields.
func (m StartupLogMaterializer) LogString(scope OperationScope, level apiobs.TelemetryLogLevel, message string) bool {
	record := apiobs.NewLogRecordString(scope.Operation(), level, message)
	return m.LogRecord(scope, record)
}

// LogRecord records a prebuilt startup log request and writes it locally only if
// the tracker accepted it. The accepted record is flagged so later background
// projection can skip duplicate local zerolog output while still exporting it to
// non-local telemetry sinks.
func (m StartupLogMaterializer) LogRecord(scope OperationScope, record apiobs.TelemetryRecord) bool {
	if scope.IsZero() || record.Kind != apiobs.TelemetryRecordLog {
		return false
	}
	if record.TimestampUnixNano == 0 {
		record.TimestampUnixNano = nowUnixNano()
	}
	record.Flags |= apiobs.TelemetryRecordFlagLocalLogMaterialized
	if !scope.Record(record) {
		return false
	}
	m.materializeLocal(scope.Ref(), record)
	return true
}

func (StartupLogMaterializer) materializeLocal(ref apiobs.OperationRef, record apiobs.TelemetryRecord) {
	event := logger.TelemetryNoCtx(record.Level)
	if event == nil {
		return
	}
	if !ref.ID.IsZero() {
		event = event.Str(apicommand.OperationID, ref.ID.String())
	}
	if !ref.ParentID.IsZero() {
		event = event.Str(parentOperationIDField, ref.ParentID.String())
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
	event.Msg(string(record.NameBytes()))
}
