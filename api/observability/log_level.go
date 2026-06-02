package observability

// TelemetryLogLevel is a deterministic numeric log level used by telemetry log
// records. Dynamic log messages and fields stay as bytes; only this fixed level
// vocabulary is represented as an integer.
type TelemetryLogLevel uint8

const (
	TelemetryLogLevelUnspecified TelemetryLogLevel = iota
	TelemetryLogLevelTrace
	TelemetryLogLevelDebug
	TelemetryLogLevelInfo
	TelemetryLogLevelWarn
	TelemetryLogLevelError
	TelemetryLogLevelFatal
	TelemetryLogLevelPanic
)

func (level TelemetryLogLevel) String() string {
	switch level {
	case TelemetryLogLevelTrace:
		return "trace"
	case TelemetryLogLevelDebug:
		return "debug"
	case TelemetryLogLevelInfo:
		return "info"
	case TelemetryLogLevelWarn:
		return "warn"
	case TelemetryLogLevelError:
		return "error"
	case TelemetryLogLevelFatal:
		return "fatal"
	case TelemetryLogLevelPanic:
		return "panic"
	default:
		return "unspecified"
	}
}
