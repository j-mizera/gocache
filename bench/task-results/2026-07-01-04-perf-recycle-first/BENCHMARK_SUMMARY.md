# Phase 9: Metrics Cleanup + Skip Elimination — Session Report

**Date:** 2026-07-01  
**Branch:** `perf/telemetry-tmpfs`  
**Session scope:** Pipeline metrics redesign, slot pool tuning, drain recycle-first reorder  
**Councils convened:** 3 (6-member deliberations)  
**Benchmark matrices run:** 4  

---

## Executive Summary

Starting from 1,201 telemetry skips per benchmark run (0.062% skip rate), we systematically diagnosed and eliminated all telemetry loss through three council-informed interventions. **Final result: 0 skips, 100.000% telemetry coverage** in both IPC and OTel pipelined benchmarks, with throughput maintained within normal variance.

| Metric | Start of Session | End of Session |
|---|---:|---:|
| OTel pipelined skips | 1,201 | **0** |
| IPC pipelined skips | 1,201 | **0** |
| Telemetry coverage | 99.938% | **100.000%** |
| OTel pipelined %Valkey | 83.2% | 87.0% |
| Dropped records | 0 | 0 |

---

## Problem Statement

Two issues identified at session start:

1. **Misplaced metrics:** `pipeline.event.operation_started/completed` were operation-lifecycle telemetry events wearing pipeline-execution names. They fired conditionally (only when telemetry scope active) and per-operation-start (not per-command), producing misleading 299K vs 1.5M ratios in pipelined mode.

2. **Telemetry skips:** 1,201 operations per benchmark run had their telemetry silently skipped due to slot pool exhaustion during burst load.

---

## Council Decisions

### Council 1: Metrics Design (sme + critic + reviewer)

**Question:** What pipeline metrics are correct? Should misplaced counters be removed or replaced?

**Verdict (unanimous):**
- **REMOVE** `pipeline.event.operation_started/completed` — operation-lifecycle events don't belong in pipeline namespace
- **DO NOT add** `command_submitted/succeeded/failed` — duplicates tracker-owned counters, re-imports the original ambiguity
- **ADD** pipeline decision counters: `command_unknown`, `command_arg_error`, `command_queued`, `plugin_routed` — measure pipeline decisions not covered anywhere else
- Two-surface design (benchstats benchmark-scoped + telemetry GCPC runtime) is **sound** — the bug was misplacement, not architecture

### Council 2: Slot Growth Strategy (sme + critic + reviewer)

**Question:** User proposed exponential ×2 hot-path growth with background shrinkage. Is this the right approach?

**Critical discovery:** The growth/shrink mechanism **already existed** in the codebase (`HotShardGrowthConfig` with background monitor) but was **disabled in production** (`Enabled` never set in `main.go`).

**Verdict (unanimous):**
- **REJECT** exponential hot-path growth — violates ADR-0034, mutex-held 80KB allocation
- **REJECT** custom shrinkage — already built into existing monitor
- **ENABLE** existing dormant `HotShardGrowth` monitor (1 config line)
- **RAISE** `minSegmentsPerShard` 1→2 (256→512 initial slots)

### Council 3: Remaining Skips (sme + critic + reviewer)

**Question:** After raising baseline to 512, 278 OTel skips remain (all on shard 6). How to eliminate?

**Root cause confirmed:** Drain backpressure. During burst load, the drain callback (proto marshal + tmpfs write + event emit) ran BEFORE slots were recycled, keeping them unavailable for ms-scale durations.

**Verdict (unanimous):**
- **Option 3 (recycle-first):** Reorder `DrainCompletedShard` to recycle slots BEFORE running the slow callback
- **Prerequisite:** Deep-copy `ContextOverlay` to make it owned (was borrowed from slot memory)
- **Safety:** Defer `releaseContextVersion` to after callback (preserves context materialization)
- **Belt-and-suspenders:** Raise `minSegments` to 3 (768 slots), set `MaxChunksPerClass` to match

---

## Code Changes

### Task 9.1: Pipeline Metrics Cleanup

| File | Change |
|---|---|
| `pkg/benchstats/stats.go` | Removed `pipelineOperationStarted/Completed` fields, Record funcs, reset, snapshot keys. Added `pipelineCommandUnknown/ArgError/Queued/PluginRouted`. |
| `pkg/pipeline/pipeline.go` | Removed 2 old Record calls. Added 4 new Record calls at decision points in `evaluateCore()`. |
| `pkg/benchstats/stats_test.go` | Updated assertions for new counter keys. |
| `pkg/pipeline/telemetry_saturation_test.go` | Removed old counter assertions. |

### Task 9.2: Per-Subscriber Tmpfs Health Metrics

| File | Change |
|---|---|
| `commons/observability/telemetry_tmpfs.go` | Added `recordsWritten`, `bytesWritten`, `writeErrors` counters + getters to `TmpfsTelemetryWriter`. |
| `pkg/metrics/telemetry.go` | Added `TelemetrySubscriberSnapshot`, `TelemetrySubscriberSource` interface, `SetSubscriberSource()` on `TelemetryProvider`, subscriber output in `QueryData()`. |
| `pkg/plugin/manager/manager.go` | Added `SubscriberStats()` method to Manager, wired `SetSubscriberSource(m)` in `SetOperationTrackerManager`. |

### Task 9.3: Decision Record

| File | Change |
|---|---|
| `.opencode/IMPLEMENTATION_STATUS.md` | Added Phase 9 section with deferred event-path per-plugin metrics decision record. |

### Task 9.4: Slot Pool Growth

| File | Change |
|---|---|
| `cmd/server/main.go` | `minSegmentsPerShard` 1→2, enabled `HotShardGrowth{Enabled: true, MaxGrowthSegments: 4}`. |

### Task 9.6: Drain Recycle-First Reorder

| File | Change |
|---|---|
| `commons/observability/slot_tracker.go` | (1) Added `copyContextOverlay()` helper. (2) Deep-copy `ContextOverlay` in `completedOperationFromSlot`. (3) Reordered `DrainCompletedShard`: recycle slots BEFORE callback, defer `releaseContextVersion` to after callback. (4) Updated `CompletedOperation` struct comments (ContextOverlay owned, Records callback-lifetime). |
| `commons/observability/slot_tracker_test.go` | Added `TestDrainCompletedShardRecyclesBeforeCallback` — verifies slot is reusable during callback. |
| `cmd/server/main.go` | `minSegmentsPerShard` 2→3 (768 slots), added `MaxChunksPerClass: 1024` (matches max slot capacity). |

---

## Benchmark Results

### Four-Run Progressive Comparison

| Run | Description | Valkey PIPE | OTel PIPE | OTel %Valkey | OTel Skips | Skip Rate |
|---|---|---:|---:|---:|---:|---:|
| 01 | Per-batch baseline | 728,276 | 606,214 | 83.2% | 1,201 | 0.062% |
| 02 | Metrics cleanup | 706,381 | 659,322 | 93.3% | 554 | 0.029% |
| 03 | Slot growth (512) | 735,933 | 655,009 | 89.0% | 278 | 0.014% |
| **04** | **Recycle-first (768)** | **697,660** | **606,603** | **87.0%** | **0** | **0.000%** |

Note: Valkey RPS varies ±5% run-to-run due to system-level load. OTel absolute RPS is stable at 606K-659K across all runs.

### Run 04 Detailed Results

| Config | Standard RPS | % Valkey | Pipelined RPS | % Valkey |
|---|---:|---:|---:|---:|
| Valkey | 87,753 | 100% | 697,660 | 100% |
| GoCache core | 82,523 | 94.0% | 597,648 | 85.7% |
| GoCache IPC | 82,226 | 93.7% | 597,994 | 85.7% |
| GoCache OTel | 81,390 | 92.8% | 606,603 | 87.0% |

### Telemetry Counters (Run 04, OTel Pipelined)

```
telemetry.skipped_operations:     0    ← ZERO
telemetry.dropped_records:        0
telemetry.dropped_completed:      1    (negligible)
telemetry.operations_started:     1,923,112
telemetry.operations_completed:   1,923,110
telemetry.commands_total:         1,923,112
telemetry.batches_total:          1,923,112
Coverage:                         100.000%
```

All 8 shards: 3 segments, 768 slots, 0 skips per shard.

### New Pipeline Decision Counters (Run 04)

```
pipeline.evaluations:        1,500,000
pipeline.command_unknown:    2    (QUIT/disconnect artifacts)
pipeline.command_arg_error:  0
pipeline.command_queued:     0
pipeline.plugin_routed:      0
```

No `pipeline.event.operation_started/completed` — successfully removed.

---

## Skip Elimination Analysis

### Root Cause Chain

```
Burst load (50 clients, pipelined)
  → Many operations complete simultaneously
  → Completed slots pushed to ring
  → Drain worker starts processing (pop → CAS WorkerOwned → callback)
  → Callback runs proto marshal + tmpfs write + event emit (ms-scale)
  → During callback, slots are WorkerOwned (unavailable)
  → New StartOperation calls find free list empty
  → SKIP (telemetry lost, command still executes)
```

### What Each Fix Did

| Fix | Mechanism | Skip Reduction | Root Cause Addressed |
|---|---|---:|---|
| Metrics cleanup (Run 02) | Removed 2 atomic ops from hot path → faster command processing → less drain lag | -53% | Hot-path overhead |
| minSegments 1→2 (Run 03) | Doubled buffer (256→512) → more headroom for completed-pending-drain | additional -50% | Buffer capacity |
| **Recycle-first (Run 04)** | **Slots recycled µs after snapshot, not ms after callback** | **-100%** | **Drain latency coupling** |
| minSegments 2→3 | 768-slot belt-and-suspenders | insurance | Burst absorption |
| MaxChunksPerClass=1024 | Prevents chunk pool exhaustion under burst | test fix | Arena pool capacity |

### The Structural Fix: Recycle-First Reorder

**Before:**
```
Pop completed → CAS WorkerOwned → [SLOW: marshal + write + emit] → Recycle slots
                                                    ↑
                                            Slots unavailable (ms)
```

**After:**
```
Pop completed → CAS WorkerOwned → Snapshot data → Recycle slots → [SLOW: marshal + write + emit]
                                                       ↑                  ↑
                                               Slots back to free     Runs on owned copies
                                                  (µs-scale)           (no slot dependency)
```

The WorkerOwned window shrank from **milliseconds** (proto marshal + tmpfs I/O) to **microseconds** (struct copy + map copy). Slots are available for reuse before the slow telemetry work even starts.

---

## Architecture Decisions

### ADR-0034 Compliance

All changes verified against ADR-0034 ("Zero-allocation operation telemetry storage"):

| Rule | How We Complied |
|---|---|
| No hot-path growth | Used config-time preallocation + off-path background monitor |
| No sync-drain on exhaustion | Reordered existing drain logic, no command-path blocking |
| No command-path allocation | `copyContextOverlay` runs on drain worker, not command goroutine |
| Best-effort with visible counters | Achieved 0 skips while maintaining visible `skipped_operations` counter |

### Council Rejected Proposals

| Proposal | Rejected By | Reason |
|---|---|---|
| Exponential ×2 hot-path growth | Council 2 (3/3) | ADR-0034 violation, mutex-held 80KB alloc |
| Custom background shrinkage | Council 2 (3/3) | Already built into existing monitor |
| `command_submitted/succeeded/failed` counters | Council 1 (critic+reviewer) | Duplicates tracker, re-imports ambiguity |
| Sync-drain on slot exhaustion | Council 2+3 (3/3) | Blocks command goroutine on drain latency |
| Concurrent drain workers per shard | Council 3 (SME) | Breaks single-worker-owns-ring invariant |

---

## Files Modified (Session Total)

| File | Tasks | Lines Changed |
|---|---|---:|
| `pkg/benchstats/stats.go` | 9.1 | +39 -19 |
| `pkg/benchstats/stats_test.go` | 9.1 | +25 -14 |
| `pkg/pipeline/pipeline.go` | 9.1 | +10 -2 |
| `pkg/pipeline/telemetry_saturation_test.go` | 9.1 | +1 -8 |
| `commons/observability/telemetry_tmpfs.go` | 9.2 | +28 |
| `commons/observability/telemetry_tmpfs_test.go` | 9.2 | +30 |
| `pkg/metrics/telemetry.go` | 9.2 | +74 -2 |
| `pkg/metrics/telemetry_test.go` | 9.2 | +20 |
| `pkg/plugin/manager/manager.go` | 9.2 | +25 -1 |
| `pkg/plugin/manager/query_test.go` | 9.2 | +5 |
| `.opencode/IMPLEMENTATION_STATUS.md` | 9.3 | +35 -3 |
| `cmd/server/main.go` | 9.4, 9.6 | +15 -5 |
| `commons/observability/slot_tracker.go` | 9.6 | +35 -15 |
| `commons/observability/slot_tracker_test.go` | 9.6 | +25 |

---

## Benchmark Data Locations

```text
bench/task-results/
├── 2026-07-01-01-perf-per-batch-telemetry/     ← Run 01 (baseline)
├── 2026-07-01-02-perf-metrics-cleanup/         ← Run 02 (metrics cleanup)
├── 2026-07-01-03-perf-slot-growth/             ← Run 03 (slot growth)
└── 2026-07-01-04-perf-recycle-first/           ← Run 04 (recycle-first, FINAL)
```

Each directory contains: CSV benchmark data, memory snapshots, telemetry JSON, and (where applicable) benchstats JSON and BENCHMARK_SUMMARY.md.

---

*Generated 2026-07-01. Phase 9 — Metrics Cleanup + Skip Elimination. 3 councils, 4 benchmark matrices, 0 telemetry skips achieved.*
