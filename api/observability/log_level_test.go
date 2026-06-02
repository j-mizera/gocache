package observability

import "testing"

func TestTelemetryLogLevelStrings(t *testing.T) {
	tests := []struct {
		level TelemetryLogLevel
		want  string
	}{
		{TelemetryLogLevelTrace, "trace"},
		{TelemetryLogLevelDebug, "debug"},
		{TelemetryLogLevelInfo, "info"},
		{TelemetryLogLevelWarn, "warn"},
		{TelemetryLogLevelError, "error"},
		{TelemetryLogLevelFatal, "fatal"},
		{TelemetryLogLevelPanic, "panic"},
		{TelemetryLogLevelUnspecified, "unspecified"},
	}
	for _, tt := range tests {
		if got := tt.level.String(); got != tt.want {
			t.Fatalf("%d.String() = %q, want %q", tt.level, got, tt.want)
		}
	}
}
