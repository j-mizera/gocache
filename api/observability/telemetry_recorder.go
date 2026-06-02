package observability

// TelemetryRecorder is the minimal capability for submitting compact telemetry
// outbox records. Implementations place requests onto a sidecar path for later
// execution and may drop records under overload, but must expose drop counts so
// incomplete telemetry is visible to callers.
type TelemetryRecorder interface {
	RecordTelemetry(TelemetryRecord) bool
	DroppedRecords() uint64
}
