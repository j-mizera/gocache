package logcollector

import (
	"strings"
	"testing"
)

func BenchmarkCollectorParseLine(b *testing.B) {
	jsonLine := []byte(`{"level":"info","source":"server","message":"command completed","_operation_id":"cmd_1","_ctx":{"_command":"SET","_arg_count":"2","shared.rex.traceparent":"00-abc-def-01"},"elapsed_ms":1}`)
	plainLine := []byte("plain plugin output")

	b.Run("json", func(b *testing.B) {
		em := &mockEmitter{}
		c := New(em)
		b.ResetTimer()
		for b.Loop() {
			c.parseLine("server", jsonLine)
		}
		c.Wait()
	})
	b.Run("plain", func(b *testing.B) {
		em := &mockEmitter{}
		c := New(em)
		b.ResetTimer()
		for b.Loop() {
			c.parseLine("plugin", plainLine)
		}
		c.Wait()
	})
	b.Run("source_reader", func(b *testing.B) {
		line := string(jsonLine) + "\n"
		b.ResetTimer()
		for b.Loop() {
			em := &mockEmitter{}
			c := New(em)
			c.AddSource("server", strings.NewReader(line))
			c.Wait()
		}
	})
}
