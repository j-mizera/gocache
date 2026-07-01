# Phase 6: Record Arena — Benchmark Summary

**Date**: 2026-06-30  
**Task**: Operation record storage rework (chunk arena + slab pool)  
**ADR**: ADR-0037 (telemetry tmpfs shared-memory IPC)  
**Hardware**: AMD Ryzen 9 7900X 12-Core, Linux amd64

## Architecture Change

Replaced flat pre-allocated `segmentSize × recordsPerOperation` array with per-operation **RecordArena** — a chained-doubling-chunk arena backed by a 3-class lock-free ChunkPool (32/64/128 records per chunk).

### Key Design Properties
- **Zero pre-allocation**: Pool grows on demand, not reserved upfront
- **Dynamic growth**: 32 → 64 → 128 → 128 → 128 chain (no fixed limit)
- **GC-opaque**: TelemetryRecord is pointer-free (~296 bytes), chunks pool-owned
- **Write-then-publish**: Data written before totalRecords.Store (snapshot-safe)
- **ABA-safe**: Mutex-protected LIFO pool (not Treiber stack)
- **Snapshot-safe reads**: SnapshotRead uses atomic totalRecords boundary + immutable chunk capacity

## Results

### Hot Path: Record Write (RecordTelemetry)

| Storage | ns/op | B/op | allocs/op |
|---------|-------|------|-----------|
| **Arena Append** | **17.8** | **0** | **0** |
| Legacy Flat Array | 12.1 | 0 | 0 |

**Verdict**: Arena append is zero-allocation (target met). 5.7ns overhead vs flat array (47%) is from localCount tracking + totalRecords.Store atomic + growth check. Negligible in context of Redis command processing (microseconds).

### Cold Path: Drain (copy-on-drain)

| Records | ns/op | B/op | allocs/op |
|---------|-------|------|-----------|
| 4 | 476 | 1,280 | 1 |
| 20 | 1,569 | 6,528 | 1 |
| 50 | 4,144 | 16,384 | 1 |
| 200 | 15,225 | 65,536 | 1 |
| 500 | 36,488 | 163,840 | 1 |

**Verdict**: Drain is 1 alloc/op (the contiguous result slice copy). Latency scales linearly (~73ns per record). Acceptable for the cold drain path.

### Full Cycle: StartOp → RecordTelemetry×N → FinishOp → Drain

| Records | ns/op | B/op | allocs/op |
|---------|-------|------|-----------|
| 4 | 952 | 1,472 | 3 |
| 20 | 2,868 | 6,720 | 3 |
| 50 | 7,175 | 16,576 | 3 |
| 200 | 24,220 | 65,730 | 3 |

**Verdict**: 3 allocs per full cycle regardless of record count (arena initial chunk + drain result slice + operation overhead). Constant alloc count confirms pool recycling works — no per-record allocations.

### Snapshot Read (concurrent-safe, active slots)

| Records | ns/op | B/op | allocs/op |
|---------|-------|------|-----------|
| 4 | 472 | 1,280 | 1 |
| 20 | 1,597 | 6,528 | 1 |
| 50 | 4,190 | 16,384 | 1 |
| 200 | 16,206 | 65,536 | 1 |

**Verdict**: SnapshotRead matches Drain performance (both copy). Uses atomic boundary — safe for concurrent reads during active appends. Verified under -race.

## Memory Comparison

| Metric | Old (flat array, rPO=1024) | New (chunk arena) |
|--------|---------------------------|-------------------|
| Pre-allocation | ~2.5 GB always | 0 (grows on demand) |
| Typical (1024 ops × 20 records) | ~2.5 GB | **~50 MB** |
| Pool ceiling (8 shards × 3 classes × 512) | N/A | ~271 MB (recycled) |
| Per-operation waste | 1017 records wasted (rPO=1024, avg 7 used) | **0** (exact fit) |
| Growth | Fixed — drops records | Dynamic — grows as needed |
| Plugin-ready | No | Yes (future MPSC via GCPC queue) |

**Memory reduction**: 98% in typical usage (2.5 GB → 50 MB).

## Race Safety

All tests pass under `-race` across 3 packages (commons/observability, pkg/server, pkg/plugin/manager):
- 158+ tests, zero data races
- Concurrent arena stress tests (16 goroutines × 1000 iterations)
- Snapshot-read-during-append tests
- Pool exhaustion + graceful drop tests

## Conclusion

The chunk arena achieves the Phase 6 goals:
1. ✅ Zero allocations on hot write path (17.8 ns/op, 0 allocs)
2. ✅ 98% memory reduction vs old flat array
3. ✅ Dynamic growth (no recordsPerOperation, no dropped records under normal load)
4. ✅ GC-opaque (pointer-free chunks, pool recycling)
5. ✅ Race-safe under -race detector
6. ✅ Plugin-ready for future multi-source telemetry

The 47% per-write overhead vs flat array (17.8 vs 12.1 ns) is acceptable — it buys dynamic growth, zero waste, and plugin readiness that the flat array cannot provide.
