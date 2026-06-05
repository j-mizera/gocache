package observability

// OperationTracker is the injectable operation-telemetry capability used by
// services and components. Implementations are responsible for keeping these
// helper methods allocation-free on their submit path.
//
// Methods submit pointer-free telemetry requests only. The sidecar stores
// telemetry state in memory for later FIFO reaping; it does not build actual
// logs, materialized events, GCPC payloads, protobuf messages, zerolog output,
// or plugin fanout on the command-executing goroutine. Reaper-side processing
// converts operation records to runtime operation output, folds context mutation
// records into worker-owned context state, and materializes logs/events later.
//
// Connection-context versioning is managed centrally by OperationTrackerManager,
// not by callers. StartOperation carries the exact ConnectionContextVersion
// current at operation start, and the operation is pinned to that version for
// worker-side processing.
type OperationTracker interface {
	TelemetryRecorder

	// StartOperation submits an operation-start request and pins the base
	// connection-context version used by later worker-side projection.
	StartOperation(operation InternalOperationIdentity, parent ParentRef, contextVersion ConnectionContextVersion) bool
	// FinishOperation submits an operation-finish request.
	FinishOperation(operation InternalOperationIdentity) bool
	// StartCommand submits a command-start request with dynamic command bytes.
	StartCommand(operation InternalOperationIdentity, commandName []byte, payload []byte) bool
	// FinishCommand submits a command-finish request with the result code.
	FinishCommand(operation InternalOperationIdentity, resultCode int64) bool
	// Log submits a request to materialize a log later; it is not itself a log.
	// Field-bearing logs should be built as TelemetryRecord values with
	// AddFieldBytes and submitted through RecordTelemetry to avoid variadic hot
	// path APIs.
	Log(operation InternalOperationIdentity, level TelemetryLogLevel, message []byte) bool
	// Event submits a request to emit a typed event later; it is not itself an event.
	Event(operation InternalOperationIdentity, eventName []byte, fields ...[]byte) bool
	// ContextUpdate submits operation context key/value deltas for worker replay.
	ContextUpdate(operation InternalOperationIdentity, pairs ...[]byte) bool
	// ContextRemove submits operation context key removals for worker replay.
	ContextRemove(operation InternalOperationIdentity, keys ...[]byte) bool
}
