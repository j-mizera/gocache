package observability

import (
	"testing"

	apiobs "gocache/api/observability"
)

const slotTrackerBenchmarkCapacity = 1 << 12

type slotTrackerCommandPath interface {
	StartOperation(apiobs.InternalOperationIdentity, apiobs.ParentRef, apiobs.ConnectionContextVersion) (InternalTrackerHandle, bool)
	RecordTelemetry(InternalTrackerHandle, apiobs.TelemetryRecord) bool
	FinishOperation(InternalTrackerHandle, SlotTerminalStatus) bool
}

func newBenchmarkSlotTracker(segmentSize, recordsPerOperation, completedRing int) *SlotOperationTrackerManager {
	return NewSlotOperationTrackerManager(SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           segmentSize,
		RecordsPerOperation:   recordsPerOperation,
		CompletedRingPerShard: completedRing,
	})
}

func drainSlotBenchmarkBatch(b *testing.B, manager *SlotOperationTrackerManager, index, capacity int) {
	b.Helper()
	if index == 0 || index%capacity != 0 {
		return
	}
	b.StopTimer()
	manager.DrainCompletedShard(0, func(CompletedOperation) {})
	b.StartTimer()
}

func BenchmarkSlotTrackerStartOperation(b *testing.B) {
	manager := newBenchmarkSlotTracker(slotTrackerBenchmarkCapacity, 1, slotTrackerBenchmarkCapacity)
	handles := make([]InternalTrackerHandle, slotTrackerBenchmarkCapacity)
	parent := apiobs.NewParentRefBytes([]byte("parent"))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%slotTrackerBenchmarkCapacity == 0 {
			b.StopTimer()
			for _, handle := range handles {
				if !manager.FinishOperation(handle, SlotTerminalFinished) {
					b.Fatal("finish during benchmark reset failed")
				}
			}
			manager.DrainCompletedShard(0, func(CompletedOperation) {})
			b.StartTimer()
		}
		operation := apiobs.InternalOperationIdentity(i + 1)
		handle, ok := manager.StartOperation(operation, parent, 0)
		if !ok {
			b.Fatal("start skipped during benchmark")
		}
		handles[i%slotTrackerBenchmarkCapacity] = handle
	}
}

func BenchmarkSlotTrackerStartOperationForConnection(b *testing.B) {
	manager := newBenchmarkSlotTracker(slotTrackerBenchmarkCapacity, 1, slotTrackerBenchmarkCapacity)
	handles := make([]InternalTrackerHandle, slotTrackerBenchmarkCapacity)
	parent := apiobs.NewParentRefBytes([]byte("parent"))
	connection := apiobs.ConnectionIdentity(1)
	if version := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme")); version.IsZero() {
		b.Fatal("benchmark connection context version should be non-zero")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%slotTrackerBenchmarkCapacity == 0 {
			b.StopTimer()
			for _, handle := range handles {
				if !manager.FinishOperation(handle, SlotTerminalFinished) {
					b.Fatal("finish during benchmark reset failed")
				}
			}
			manager.DrainCompletedShard(0, func(CompletedOperation) {})
			b.StartTimer()
		}
		operation := apiobs.InternalOperationIdentity(i + 1)
		handle, pinned, ok := manager.StartOperationForConnection(operation, parent, connection)
		if !ok {
			b.Fatal("start skipped during benchmark")
		}
		if pinned.IsZero() {
			b.Fatal("start did not pin connection context version")
		}
		handles[i%slotTrackerBenchmarkCapacity] = handle
	}
}

func BenchmarkSlotTrackerStartOperationWithConnectionContext(b *testing.B) {
	manager := newBenchmarkSlotTracker(slotTrackerBenchmarkCapacity, 1, slotTrackerBenchmarkCapacity)
	handles := make([]InternalTrackerHandle, slotTrackerBenchmarkCapacity)
	parent := apiobs.NewParentRefBytes([]byte("parent"))
	connection := apiobs.ConnectionIdentity(1)
	if version := manager.UpdateConnectionContext(connection, []byte("tenant"), []byte("acme")); version.IsZero() {
		b.Fatal("benchmark connection context version should be non-zero")
	}
	cmdMeta := map[string]string{"tenant": "globex", "traceparent": "00-abc"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%slotTrackerBenchmarkCapacity == 0 {
			b.StopTimer()
			for _, handle := range handles {
				if !manager.FinishOperation(handle, SlotTerminalFinished) {
					b.Fatal("finish during benchmark reset failed")
				}
			}
			manager.DrainCompletedShard(0, func(CompletedOperation) {})
			b.StartTimer()
		}
		operation := apiobs.InternalOperationIdentity(i + 1)
		handle, pinned, ok := manager.StartOperationWithConnectionContext(operation, parent, connection, cmdMeta)
		if !ok {
			b.Fatal("start skipped during benchmark")
		}
		if pinned.IsZero() {
			b.Fatal("start did not pin connection context version")
		}
		handles[i%slotTrackerBenchmarkCapacity] = handle
	}
}

func BenchmarkSlotTrackerRecordTelemetry(b *testing.B) {
	manager := newBenchmarkSlotTracker(1, slotTrackerBenchmarkCapacity, 1)
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		b.Fatal("start skipped before benchmark")
	}
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%slotTrackerBenchmarkCapacity == 0 {
			b.StopTimer()
			if !manager.FinishOperation(handle, SlotTerminalFinished) {
				b.Fatal("finish during benchmark reset failed")
			}
			manager.DrainCompletedShard(0, func(CompletedOperation) {})
			var started bool
			handle, started = manager.StartOperation(1, apiobs.ParentRef{}, 0)
			if !started {
				b.Fatal("restart during benchmark reset skipped")
			}
			b.StartTimer()
		}
		if !manager.RecordTelemetry(handle, record) {
			b.Fatal("record dropped during benchmark")
		}
	}
}

func BenchmarkSlotTrackerFinishOperation(b *testing.B) {
	manager := newBenchmarkSlotTracker(slotTrackerBenchmarkCapacity, 1, slotTrackerBenchmarkCapacity)
	handles := startSlotBenchmarkHandles(b, manager, slotTrackerBenchmarkCapacity)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%slotTrackerBenchmarkCapacity == 0 {
			b.StopTimer()
			manager.DrainCompletedShard(0, func(CompletedOperation) {})
			handles = startSlotBenchmarkHandles(b, manager, slotTrackerBenchmarkCapacity)
			b.StartTimer()
		}
		if !manager.FinishOperation(handles[i%slotTrackerBenchmarkCapacity], SlotTerminalFinished) {
			b.Fatal("finish failed during benchmark")
		}
	}
}

func BenchmarkSlotTrackerNoSlotStart(b *testing.B) {
	manager := newBenchmarkSlotTracker(1, 1, 1)
	if _, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0); !ok {
		b.Fatal("initial start skipped before benchmark")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := manager.StartOperation(apiobs.InternalOperationIdentity(i+2), apiobs.ParentRef{}, 0); ok {
			b.Fatal("no-slot start unexpectedly succeeded")
		}
	}
}

func BenchmarkSlotTrackerRecordOverflow(b *testing.B) {
	manager := newBenchmarkSlotTracker(1, 1, 1)
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		b.Fatal("start skipped before benchmark")
	}
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)
	if !manager.RecordTelemetry(handle, record) {
		b.Fatal("initial record failed before benchmark")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if manager.RecordTelemetry(handle, record) {
			b.Fatal("overflow record unexpectedly fit")
		}
	}
}

func BenchmarkSlotTrackerCompletedRingOverflow(b *testing.B) {
	manager := newBenchmarkSlotTracker(slotTrackerBenchmarkCapacity+1, 1, 1)
	fillCompletedRingForBenchmark(b, manager)
	handles := startSlotBenchmarkHandles(b, manager, slotTrackerBenchmarkCapacity)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%slotTrackerBenchmarkCapacity == 0 {
			b.StopTimer()
			manager.DrainCompletedShard(0, func(CompletedOperation) {})
			fillCompletedRingForBenchmark(b, manager)
			handles = startSlotBenchmarkHandles(b, manager, slotTrackerBenchmarkCapacity)
			b.StartTimer()
		}
		if manager.FinishOperation(handles[i%slotTrackerBenchmarkCapacity], SlotTerminalFailed) {
			b.Fatal("completed-ring overflow finish unexpectedly enqueued")
		}
	}
}

func BenchmarkSlotTrackerInterfaceOperationLifecycle(b *testing.B) {
	concrete := newBenchmarkSlotTracker(slotTrackerBenchmarkCapacity, 2, slotTrackerBenchmarkCapacity)
	var manager slotTrackerCommandPath = concrete
	parent := apiobs.NewParentRefBytes([]byte("parent"))
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, 1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drainSlotBenchmarkBatch(b, concrete, i, slotTrackerBenchmarkCapacity)
		operation := apiobs.InternalOperationIdentity(i + 1)
		handle, ok := manager.StartOperation(operation, parent, 0)
		if !ok {
			b.Fatal("start skipped during benchmark")
		}
		record.Operation = operation
		if !manager.RecordTelemetry(handle, record) {
			b.Fatal("record failed during benchmark")
		}
		if !manager.FinishOperation(handle, SlotTerminalFinished) {
			b.Fatal("finish failed during benchmark")
		}
	}
}

func startSlotBenchmarkHandles(b *testing.B, manager *SlotOperationTrackerManager, count int) []InternalTrackerHandle {
	b.Helper()
	handles := make([]InternalTrackerHandle, count)
	for i := range handles {
		handle, ok := manager.StartOperation(apiobs.InternalOperationIdentity(i+1), apiobs.ParentRef{}, 0)
		if !ok {
			b.Fatal("start skipped while preparing benchmark handles")
		}
		handles[i] = handle
	}
	return handles
}

func fillCompletedRingForBenchmark(b *testing.B, manager *SlotOperationTrackerManager) {
	b.Helper()
	handle, ok := manager.StartOperation(0, apiobs.ParentRef{}, 0)
	if !ok {
		b.Fatal("start skipped while filling completed ring")
	}
	if !manager.FinishOperation(handle, SlotTerminalFinished) {
		b.Fatal("finish failed while filling completed ring")
	}
}

func BenchmarkOperationScopeContextUpdateStrings(b *testing.B) {
	manager := newBenchmarkSlotTracker(1, slotTrackerBenchmarkCapacity, 1)
	scope := startOperationScopeBenchmark(b, manager)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%slotTrackerBenchmarkCapacity == 0 {
			b.StopTimer()
			if !scope.Finish(SlotTerminalFinished) {
				b.Fatal("finish during benchmark reset failed")
			}
			manager.DrainCompletedShard(0, func(CompletedOperation) {})
			scope = startOperationScopeBenchmark(b, manager)
			b.StartTimer()
		}
		if !scope.ContextUpdateStrings("tenant", "acme", "role", "reader") {
			b.Fatal("context.update dropped during benchmark")
		}
	}
}

func BenchmarkOperationScopeContextRemoveStrings(b *testing.B) {
	manager := newBenchmarkSlotTracker(1, slotTrackerBenchmarkCapacity, 1)
	scope := startOperationScopeBenchmark(b, manager)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i != 0 && i%slotTrackerBenchmarkCapacity == 0 {
			b.StopTimer()
			if !scope.Finish(SlotTerminalFinished) {
				b.Fatal("finish during benchmark reset failed")
			}
			manager.DrainCompletedShard(0, func(CompletedOperation) {})
			scope = startOperationScopeBenchmark(b, manager)
			b.StartTimer()
		}
		if !scope.ContextRemoveStrings("tenant", "role") {
			b.Fatal("context.remove dropped during benchmark")
		}
	}
}

func startOperationScopeBenchmark(b *testing.B, manager *SlotOperationTrackerManager) OperationScope {
	b.Helper()
	handle, ok := manager.StartOperation(1, apiobs.ParentRef{}, 0)
	if !ok {
		b.Fatal("start skipped before benchmark")
	}
	return NewOperationScope(manager, handle, 1, apiobs.OperationRef{})
}
