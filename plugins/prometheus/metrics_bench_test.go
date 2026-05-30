package main

import (
	"bytes"
	"context"
	"testing"

	apiEvents "gocache/api/events"
)

func BenchmarkCollectorRecord(b *testing.B) {
	collector := NewCollector()
	for b.Loop() {
		collector.Record("SET", 42_000, false)
	}
}

func BenchmarkCollectorWritePrometheus(b *testing.B) {
	collector := NewCollector()
	commands := []string{"GET", "SET", "HSET", "LPUSH", "SADD"}
	for i := 0; i < 1_000; i++ {
		collector.Record(commands[i%len(commands)], uint64(1_000+i), i%17 == 0)
	}
	var buf bytes.Buffer
	b.ResetTimer()
	for b.Loop() {
		buf.Reset()
		collector.WritePrometheus(&buf, pluginName, "bench")
	}
}

func BenchmarkPrometheusHandleCommandCompletedEvent(b *testing.B) {
	plugin := &prometheusPlugin{collector: NewCollector()}
	evt := apiEvents.NewCommandCompleted("SET", []string{"k", "v"}, 42_000, "OK", "", nil).Proto
	for b.Loop() {
		plugin.HandleEvent(context.Background(), evt)
	}
}
