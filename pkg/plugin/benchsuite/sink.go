package benchsuite

// SuiteVersion marks the benchmark suite version for output metadata.
// v1 = pre-AOF baseline. All fire-and-forget numbers are scoped as
// "send-cost only" and become meaningless post-AOF. When AOF lands,
// bump to v2 and re-baseline.
const SuiteVersion = "v1-pre-aof"

// SuiteScope documents the scope of v1 numbers. Fire-and-forget send-cost
// baselines must not be cited as delivery latency after AOF implementation.
const SuiteScope = "pre-AOF baseline; fire-and-forget numbers are send-cost only"

// EventSink is the abstraction for telemetry/event delivery backends.
// The benchmark suite targets this interface so tmpfs and future AOF
// backends can be compared without suite redesign.
//
// Current implementation: TmpfsTelemetryWriter in commons/observability/
// already satisfies this interface (Write([]byte) (int, error), Close() error).
// When AOF is implemented, a new AOFEventSink will implement the same
// interface, enabling before/after comparison with the same benchmark code.
type EventSink interface {
	// Write delivers data to the sink. Returns bytes written.
	Write(data []byte) (int, error)
	// Close releases sink resources.
	Close() error
}

// Compile-time interface compliance check.
// TmpfsTelemetryWriter already implements Write and Close, but lives in
// commons/observability which has a linux build constraint. This check
// is documented here for future implementors rather than enforced at
// compile time to avoid import cycles.
