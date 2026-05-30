package manager

import (
	"testing"

	apiEvents "gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"

	"google.golang.org/protobuf/proto"
)

func BenchmarkFilterEventContext(b *testing.B) {
	evt := apiEvents.NewOperationCompleted("cmd_1", "command", 42_000, "completed", "", map[string]string{
		"_command":               "SET",
		"_arg_count":             "2",
		"shared.rex.traceparent": "00-abc-def-01",
		"prometheus.visible":     "yes",
		"other.hidden":           "no",
	}).Proto

	b.Run("clone_then_project", func(b *testing.B) {
		for b.Loop() {
			cloned := proto.Clone(evt).(*gcpcv1.EventV1)
			_ = projectEventForPlugin(cloned, "prometheus")
		}
	})

	b.Run("project_direct", func(b *testing.B) {
		for b.Loop() {
			_ = projectEventForPlugin(evt, "prometheus")
		}
	})
}
