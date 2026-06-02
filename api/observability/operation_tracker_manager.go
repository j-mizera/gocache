package observability

// ConnectionContextVisitor receives immutable connection-context key/value pairs
// for a specific ConnectionContextVersion. Implementations call it off the
// command hot path while worker-side processing materializes telemetry context.
type ConnectionContextVisitor func(key, value string) bool

// OperationTrackerManager is the injectable telemetry manager capability for
// core components that need to submit requests for logs, events, commands, or
// operation context mutations for an internal operation identity.
//
// Implementations are configured with a fixed shard count and pre-create their
// shard trackers during initialization. Get must be a cheap routing operation:
// compute the shard from the internal operation identity and return the existing
// tracker for that shard. It must not lazily allocate trackers or take a mutex
// on the command/log/event submit path.
//
// Connection context versions are created by the manager, not by callers. A
// started operation carries the exact ConnectionContextVersion current at start.
// Later worker-side processing resolves that same immutable version, even if the
// connection context changed while the operation was running.
type OperationTrackerManager interface {
	Get(operation InternalOperationIdentity) OperationTracker

	UpdateConnectionContext(connection ConnectionIdentity, pairs ...[]byte) ConnectionContextVersion
	RemoveConnectionContext(connection ConnectionIdentity, keys ...[]byte) ConnectionContextVersion
	PinCurrentConnectionContextVersion(connection ConnectionIdentity) ConnectionContextVersion
	RetainConnectionContextVersion(version ConnectionContextVersion) bool
	ReleaseConnectionContextVersion(version ConnectionContextVersion) bool
	VisitConnectionContextVersion(version ConnectionContextVersion, visitor ConnectionContextVisitor) bool

	ShardCount() int
	DroppedRecords() uint64
}
