package metrics

import (
	"sync"
	"testing"
)

func TestCommandCollectorRecordsOnlyWithActiveConsumer(t *testing.T) {
	collector := NewCommandCollector()

	collector.RecordCommand("PING", 1_000, false)
	if got := collector.Snapshot(); len(got) != 0 {
		t.Fatalf("snapshot before consumer = %#v, want empty", got)
	}

	collector.AddConsumer()
	collector.RecordCommand("PING", 1_000, false)
	collector.RecordCommand("PING", 2_000_000, true)

	snap := collector.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("len(snapshot)=%d, want 1", len(snap))
	}
	if snap[0].Command != "PING" {
		t.Fatalf("command=%q, want PING", snap[0].Command)
	}
	if snap[0].Total != 2 {
		t.Fatalf("total=%d, want 2", snap[0].Total)
	}
	if snap[0].Errors != 1 {
		t.Fatalf("errors=%d, want 1", snap[0].Errors)
	}
	if snap[0].SumNs != 2_001_000 {
		t.Fatalf("sum_ns=%d, want 2001000", snap[0].SumNs)
	}

	collector.RemoveConsumer()
	collector.RecordCommand("PING", 1_000, false)
	if snap := collector.Snapshot(); snap[0].Total != 2 {
		t.Fatalf("total after consumer removal=%d, want 2", snap[0].Total)
	}
}

func TestCommandCollectorSnapshotIsDeterministicAndCopied(t *testing.T) {
	collector := NewCommandCollector()
	collector.AddConsumer()
	collector.RecordCommand("SET", 1_000, false)
	collector.RecordCommand("GET", 1_000, false)

	snap := collector.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len(snapshot)=%d, want 2", len(snap))
	}
	if snap[0].Command != "GET" || snap[1].Command != "SET" {
		t.Fatalf("snapshot order=%q,%q; want GET,SET", snap[0].Command, snap[1].Command)
	}

	snap[0].Counts[0] = 99
	fresh := collector.Snapshot()
	if fresh[0].Counts[0] != 1 {
		t.Fatalf("snapshot shared counts slice: got %d, want 1", fresh[0].Counts[0])
	}
}

func TestCommandCollectorDrainsConcurrentProducersFromSidecarRing(t *testing.T) {
	collector := newCommandCollector(1024)
	collector.AddConsumer()

	const producers = 8
	const perProducer = 32
	var wg sync.WaitGroup
	wg.Add(producers)
	for i := 0; i < producers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perProducer; j++ {
				collector.RecordCommand("PING", 1_000, j%2 == 0)
			}
		}()
	}
	wg.Wait()

	snap := collector.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("len(snapshot)=%d, want 1", len(snap))
	}
	wantTotal := uint64(producers * perProducer)
	if snap[0].Total != wantTotal {
		t.Fatalf("total=%d, want %d", snap[0].Total, wantTotal)
	}
	if snap[0].Errors != wantTotal/2 {
		t.Fatalf("errors=%d, want %d", snap[0].Errors, wantTotal/2)
	}
	if collector.DroppedRecords() != 0 {
		t.Fatalf("dropped records=%d, want 0", collector.DroppedRecords())
	}
}

func TestCommandCollectorDropsRatherThanBlockingWhenRingFull(t *testing.T) {
	collector := newCommandCollector(1)
	collector.AddConsumer()

	collector.RecordCommand("PING", 1_000, false)
	collector.RecordCommand("PING", 1_000, false)

	snap := collector.Snapshot()
	if len(snap) != 1 || snap[0].Total != 1 {
		t.Fatalf("snapshot=%#v, want one accepted record", snap)
	}
	if collector.DroppedRecords() != 1 {
		t.Fatalf("dropped records=%d, want 1", collector.DroppedRecords())
	}
}
