package operations

import "testing"

func BenchmarkOperationNew(b *testing.B) {
	for b.Loop() {
		_ = New(TypeCommand, "conn_1")
	}
}

func BenchmarkOperationEnrich(b *testing.B) {
	op := New(TypeCommand, "conn_1")
	b.ResetTimer()
	for b.Loop() {
		op.Enrich("_command", "PING")
	}
}

func BenchmarkOperationContextSnapshot(b *testing.B) {
	op := New(TypeCommand, "conn_1")
	op.Enrich("_command", "SET")
	op.Enrich("_arg_count", "2")
	op.Enrich("shared.rex.traceparent", "00-abc-def-01")
	op.Enrich("secret.token", "hidden")

	b.Run("raw", func(b *testing.B) {
		for b.Loop() {
			_ = op.ContextSnapshot(false)
		}
	})
	b.Run("redacted", func(b *testing.B) {
		for b.Loop() {
			_ = op.ContextSnapshot(true)
		}
	})
	b.Run("filtered", func(b *testing.B) {
		for b.Loop() {
			_ = op.FilteredContext("prometheus", true)
		}
	})
}
