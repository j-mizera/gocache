package observability

import (
	"sync/atomic"
	"testing"

	apiobs "gocache/api/observability"
)

func BenchmarkTelemetryRecordSubmit(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)

	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)
	record.SetName([]byte("GET"))
	record.SetPayload([]byte("key:1"))
	capacity := len(recorder.ring.records)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		if !recorder.RecordTelemetry(record) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerCommandDynamicName(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	tracker := newOperationTracker(recorder)
	capacity := len(recorder.ring.records)

	commandName := []byte("PLUGIN.CUSTOM")
	payload := []byte("arg1 arg2")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		if !tracker.StartCommand(1, commandName, payload) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerLogFields(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	tracker := newOperationTracker(recorder)
	capacity := len(recorder.ring.records)

	message := []byte("operation finished")
	key1, value1 := []byte("status"), []byte("ok")
	key2, value2 := []byte("tenant"), []byte("acme")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		record := apiobs.NewLogRecordBytes(1, apiobs.TelemetryLogLevelDebug, message)
		record.AddFieldBytes(key1, value1)
		record.AddFieldBytes(key2, value2)
		if !tracker.RecordTelemetry(record) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerContextUpdate(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	tracker := newOperationTracker(recorder)
	capacity := len(recorder.ring.records)

	key1, value1 := []byte("tenant"), []byte("acme")
	key2, value2 := []byte("role"), []byte("reader")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		if !tracker.ContextUpdate(1, key1, value1, key2, value2) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerInterfaceOperationLifecycle(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	var tracker apiobs.OperationTracker = newOperationTracker(recorder)
	capacity := len(recorder.ring.records)
	parent := apiobs.NewParentRefBytes([]byte("parent-operation"))
	version := apiobs.ConnectionContextVersion(3)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i*2, capacity)
		operation := apiobs.InternalOperationIdentity(i + 1)
		if !tracker.StartOperation(operation, parent, version) {
			b.Fatal("start record dropped during benchmark")
		}
		if !tracker.FinishOperation(operation) {
			b.Fatal("finish record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerInterfaceFinishCommand(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	var tracker apiobs.OperationTracker = newOperationTracker(recorder)
	capacity := len(recorder.ring.records)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		if !tracker.FinishCommand(1, 0) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerInterfaceLogFields(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	var tracker apiobs.OperationTracker = newOperationTracker(recorder)
	capacity := len(recorder.ring.records)

	message := []byte("operation finished")
	key1, value1 := []byte("status"), []byte("ok")
	key2, value2 := []byte("tenant"), []byte("acme")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		record := apiobs.NewLogRecordBytes(1, apiobs.TelemetryLogLevelDebug, message)
		record.AddFieldBytes(key1, value1)
		record.AddFieldBytes(key2, value2)
		if !tracker.RecordTelemetry(record) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerInterfaceEventFields(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	var tracker apiobs.OperationTracker = newOperationTracker(recorder)
	capacity := len(recorder.ring.records)

	eventName := []byte("custom.plugin.event")
	key1, value1 := []byte("status"), []byte("ok")
	key2, value2 := []byte("tenant"), []byte("acme")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		if !tracker.Event(1, eventName, key1, value1, key2, value2) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerInterfaceContextUpdate(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	var tracker apiobs.OperationTracker = newOperationTracker(recorder)
	capacity := len(recorder.ring.records)

	key1, value1 := []byte("tenant"), []byte("acme")
	key2, value2 := []byte("role"), []byte("reader")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		if !tracker.ContextUpdate(1, key1, value1, key2, value2) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryTrackerInterfaceContextRemove(b *testing.B) {
	recorder := newSingleProducerRecorder(1 << 16)
	var tracker apiobs.OperationTracker = newOperationTracker(recorder)
	capacity := len(recorder.ring.records)

	key1, key2 := []byte("tenant"), []byte("role")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainBenchmarkBatch(b, recorder, i, capacity)
		if !tracker.ContextRemove(1, key1, key2) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkTelemetryManagerInterfaceGetStartCommand(b *testing.B) {
	const capacity = 1 << 16
	concrete := NewShardedOperationTrackerManager(16, capacity)
	var manager apiobs.OperationTrackerManager = concrete
	operation := apiobs.InternalOperationIdentity(42)
	shard := shardIndex(operation, concrete.ShardCount())

	commandName := []byte("PLUGIN.CUSTOM")
	payload := []byte("arg1 arg2")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%capacity == 0 {
			b.StopTimer()
			concrete.DrainShard(shard, func(apiobs.TelemetryRecord) {})
			b.StartTimer()
		}
		if !manager.Get(operation).StartCommand(operation, commandName, payload) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkShardedManagerGetPrecreated(b *testing.B) {
	manager := NewShardedOperationTrackerManager(16, 1<<16)
	identity := apiobs.InternalOperationIdentity(42)
	_ = manager.Get(identity)
	var sink apiobs.OperationTracker
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sink = manager.Get(identity)
	}
	_ = sink
}

func BenchmarkShardIndex(b *testing.B) {
	identity := apiobs.InternalOperationIdentity(42)
	var sink int64
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		atomic.AddInt64(&sink, int64(shardIndex(identity, 8)))
	}
}

func drainBenchmarkBatch(b *testing.B, recorder telemetryRecorder, index, capacity int) {
	b.Helper()
	if index == 0 || index%capacity != 0 {
		return
	}
	b.StopTimer()
	recorder.drain(func(apiobs.TelemetryRecord) {})
	b.StartTimer()
}
