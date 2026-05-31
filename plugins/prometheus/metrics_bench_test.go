package main

import (
	"bytes"
	"strconv"
	"testing"
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

func BenchmarkCollectorReplaceFromQuery(b *testing.B) {
	collector := NewCollector()
	data := map[string]string{
		"buckets.count":    strconv.Itoa(len(defaultBuckets)),
		"commands.count":   "1",
		"command.0.name":   "SET",
		"command.0.total":  "1000",
		"command.0.errors": "17",
		"command.0.sum_ns": "42000000",
	}
	for i := 0; i < len(defaultBuckets)+1; i++ {
		data["command.0.bucket."+strconv.Itoa(i)] = "100"
	}
	for b.Loop() {
		if err := collector.ReplaceFromQuery(data); err != nil {
			b.Fatal(err)
		}
	}
}
