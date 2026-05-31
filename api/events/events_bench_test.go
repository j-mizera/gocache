package events

import (
	"testing"

	gcpc "gocache/api/gcpc/v1"
)

var benchmarkMetadata = map[string]string{
	"shared.rex.traceparent": "00-abc-def-01",
	"_command":               "SET",
	"_arg_count":             "2",
}

func BenchmarkEventConstructors(b *testing.B) {
	args := []string{"key", "value"}
	ctx := map[string]string{
		"_command":               "SET",
		"_arg_count":             "2",
		"shared.rex.traceparent": "00-abc-def-01",
	}

	b.Run("command_started", func(b *testing.B) {
		for b.Loop() {
			_ = NewCommandStarted("SET", args, benchmarkMetadata).WithOperationID("cmd_1")
		}
	})
	b.Run("command_completed", func(b *testing.B) {
		for b.Loop() {
			_ = NewCommandCompleted("SET", args, 42_000, "OK", "", benchmarkMetadata).WithOperationID("cmd_1")
		}
	})
	b.Run("operation_started", func(b *testing.B) {
		for b.Loop() {
			_ = NewOperationStarted("cmd_1", "command", "conn_1", ctx)
		}
	})
	b.Run("operation_completed", func(b *testing.B) {
		for b.Loop() {
			_ = NewOperationCompleted("cmd_1", "command", 42_000, "completed", "", ctx)
		}
	})
	records := []*gcpc.RuntimeLogRecordV1{{
		OperationId: "cmd_1",
		Level:       "info",
		Message:     "command completed",
		Fields:      ctx,
	}}
	b.Run("runtime_log_batch", func(b *testing.B) {
		for b.Loop() {
			_ = NewRuntimeLogBatch(records)
		}
	})
}
