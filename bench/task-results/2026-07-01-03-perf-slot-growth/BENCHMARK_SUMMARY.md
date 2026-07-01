# GoCache Benchmark Summary: Metrics Cleanup + Slot Pool Growth (Phase 9)

**Date:** 2026-07-01  
**Branch:** `perf/telemetry-tmpfs`  
**Runs:** 3 consecutive benchmark matrices on the same day  
**Hardware:** AMD Ryzen 9 7900X 12-core/24-thread, 8 target + 4 client cores

---

## 1. Executive Summary

Three benchmark matrices were run to validate Phase 9 changes: (1) per-batch telemetry baseline, (2) metrics cleanup (removed misplaced counters, added pipeline decision counters), and (3) slot pool growth (raised baseline 256→512, enabled background growth monitor).

### Key Results (Run 03 — Slot Growth, final)

| Config | Standard RPS | % Valkey | Pipelined RPS | % Valkey | Skips (PIPE) |
|---|---:|---:|---:|---:|---:|
| **Valkey** | **93,446** | 100% | **735,933** | 100% | — |
| GoCache core | 85,846 | 91.9% | 668,545 | 90.8% | — |
| GoCache IPC | 85,794 | 91.8% | 623,677 | 84.8% | **0** |
| GoCache OTel | 87,053 | 93.2% | 655,009 | 89.0% | **278** |

### Skip Elimination Progress

| Run | IPC Skips | OTel Skips | Total Skip Rate | Change |
|---|---:|---:|---:|---|
| Jul 01-01 (per-batch) | 1,201 | 1,201 | 0.062% | baseline |
| Jul 01-02 (metrics cleanup) | 149 | 554 | 0.029% | -53% |
| **Jul 01-03 (slot growth)** | **0** | **278** | **0.014%** | **-77% vs 01-01** |

**IPC pipelined: ZERO SKIPS.** OTel pipelined skip rate: 0.014% (1 in 6,917 operations).

---

## 2. Skip Root Cause Analysis

### What Causes Skips

When `StartOperation` can't find a free slot (all are active or completed-pending-drain), it skips the operation's telemetry recording. The command still executes — only telemetry is lost.

### Root Cause: Drain Backpressure (not capacity overflow)

With 50 clients and 8 shards (~6 concurrent operations per shard average), genuine capacity overflow is impossible. The 278 remaining skips are **drain backpressure**: during burst load, completed slots accumulate faster than the drain worker can recycle them, momentarily exhausting the free list.

Evidence:
- Background growth monitor stayed at baseline (2 segments = 512 slots) — no sustained pressure detected
- Skips are concentrated on single shards (shard 6 in OTel run) — not uniform across all shards
- Skips correlate with OTel overhead (event fanout + tmpfs write) — IPC (no OTel overhead) has 0 skips

### What Fixed It

| Change | Mechanism | Skip Reduction |
|---|---|---:|
| Metrics cleanup (Run 02) | Removed 2 atomic ops from hot path → faster command processing → less drain lag | -53% |
| minSegments 1→2 (Run 03) | Doubled preallocated slots (256→512) → 2× buffer for completed-pending-drain | additional -50% |
| Background growth monitor | Enabled but never triggered (bursts < 300ms hysteresis window) | 0% (insurance) |

---

## 3. Methodology

### Test Configuration

| Parameter | Value |
|---|---:|
| Operations per test (`BENCH_N`) | 100,000 |
| Clients (`BENCH_CLIENTS`) | 50 |
| Keyspace (`BENCH_KEYSPACE`) | 100,000 |
| Pipeline depth (`BENCH_PIPELINE`) | 10 |
| Target CPUs | `0-7` (8 physical cores) |
| Client CPUs | `8-11` (4 physical cores) |
| Container memory limit | `2g` |
| Slot pool config (Run 03) | minSeg=2, maxSeg=4, segSize=256, HotShardGrowth=enabled |

### Configurations Tested

| Configuration | Label stem | Notes |
|---|---|---|
| Valkey | `batch-valkey` | Reference server |
| GoCache core | `batch-gocache` | No IPC plugins |
| GoCache IPC | `batch-gocache-ipc` | Prometheus IPC plugin |
| GoCache OTel | `batch-otel-gocache-ipc-otel` | Prometheus + instrumentation + OTel Collector |

---

## 4. Three-Run Comparison

### 4.1 Throughput Evolution

| Config | Run 01 PIPE | Run 02 PIPE | Run 03 PIPE | Trend |
|---|---:|---:|---:|---|
| Valkey | 728,276 | 706,381 | 735,933 | system variance ±4% |
| Core | 596,581 | 630,677 | 668,545 | improving (+12%) |
| IPC | 598,413 | 595,610 | 623,677 | improving (+4%) |
| OTel | 606,214 | 659,322 | 655,009 | stable after cleanup |

### 4.2 % of Valkey (Pipelined)

| Config | Run 01 | Run 02 | Run 03 |
|---|---:|---:|---:|
| Core | 81.9% | 89.3% | 90.8% |
| IPC | 82.2% | 84.3% | 84.8% |
| OTel | 83.2% | 93.3% | 89.0% |

Note: Run-to-run Valkey variance (±4%) affects relative percentages. OTel Run 03 absolute RPS (655K) is essentially unchanged from Run 02 (659K) — the % drop is because Valkey was faster in Run 03 (736K vs 706K).

### 4.3 Skip Elimination

| Run | Config | Skips | Rate | per Shard Distribution |
|---|---|---:|---:|---|
| 01 | IPC | 1,201 | 0.062% | shards 0,4,6,7 |
| 01 | OTel | 1,201 | 0.062% | shards 0,2,3,4,6,7 |
| 02 | IPC | 149 | 0.008% | shard 0,4,6 |
| 02 | OTel | 554 | 0.029% | shards 0,2,3,4,6,7 |
| **03** | **IPC** | **0** | **0.000%** | — |
| **03** | **OTel** | **278** | **0.014%** | shard 6 only |

### 4.4 Memory Usage (Run 03)

| Config | Baseline | Final | Delta |
|---|---:|---:|---:|
| Valkey | 9.2 MB | 29.1 MB | +19.9 MB |
| GoCache core | 42.8 MB | 304.0 MB | +261.2 MB |
| GoCache IPC | 50.7 MB | 254.3 MB | +203.6 MB |
| GoCache OTel | 50.6 MB | 272.7 MB | +222.2 MB |

Memory delta from slot pool doubling: ~640KB (256 extra slots × 8 shards × ~300 bytes). Negligible.

### 4.5 Slot Pool State (Run 03, Post-Benchmark)

All shards remained at baseline (2 segments = 512 slots). Background growth monitor detected no sustained pressure. No segments were grown or retired during the benchmark.

---

## 5. Tracker Counters (Run 03, OTel Pipelined)

| Counter | Value |
|---|---:|
| `telemetry.commands_total` | 1,922,834 |
| `telemetry.batches_total` | 1,923,112 |
| `telemetry.operations_started` | 1,922,834 |
| `telemetry.operations_completed` | 1,922,832 |
| `telemetry.skipped_operations` | **278** |
| `telemetry.dropped_records` | **0** |
| `telemetry.dropped_completed` | 1 |
| Coverage | **99.986%** |

---

## 6. Remaining 278 Skips Analysis

All 278 remaining skips are on **shard 6 only**. This shard handles a subset of the keyspace that experienced burst load during the OTel benchmark. The OTel path adds drain overhead (event bus fanout + tmpfs write per completed operation), making this shard's drain worker momentarily fall behind.

### Options to Eliminate Remaining Skips

| Option | Expected Impact | Effort | Risk |
|---|---|---|---|
| Raise minSegments to 3 (768 slots) | Likely eliminates most | 1 line | Very low |
| Reduce drain interval (10ms→5ms) | Faster slot recycling | 1 line | Low (more CPU) |
| Add drain worker nudge on skip | Force-drain before skip | Code change | Medium |
| Accept 0.014% skip rate | — | 0 | None |

**Recommendation:** Accept 0.014%. This is 1 skipped telemetry record per 6,917 operations — within the acceptable best-effort telemetry contract defined by ADR-0034.

---

## 7. Pipeline Decision Counters (NEW, Run 03)

| Counter | Value (PIPE) | Notes |
|---|---:|---|
| `pipeline.evaluations` | 1,500,000 | Per command, always fires |
| `pipeline.command_unknown` | 2 | QUIT/disconnect artifacts |
| `pipeline.command_arg_error` | 0 | No arg validation failures |
| `pipeline.command_queued` | 0 | No transactions |
| `pipeline.plugin_routed` | 0 | No plugin-routed commands |

No `pipeline.event.operation_started/completed` — successfully removed.

---

## 8. Phase 9 Changes Summary

| Task | Change | Impact |
|---|---|---|
| 9.1 | Removed `pipeline.event.operation_started/completed`, added 4 decision counters | -53% skips (hot-path atomic ops removed) |
| 9.2 | Added per-subscriber tmpfs health metrics | New diagnostics: records_written, write_errors per subscriber |
| 9.3 | Decision record for deferred event-path per-plugin counters | Documentation |
| 9.4 | Raised minSegments 1→2, enabled HotShardGrowth monitor | Additional -50% skips (IPC: 0, OTel: 278) |

---

## 9. Raw Data Files

```text
bench/task-results/2026-07-01-03-perf-slot-growth/
├── batch-valkey-valkey.csv / -pipelined.csv / -memory.txt
├── batch-gocache-gocache.csv / -pipelined.csv / -memory.txt
├── batch-gocache-ipc-gocache-ipc.csv / -pipelined.csv / -memory.txt / -config.yaml
├── batch-gocache-ipc-gocache-ipc-telemetry-{baseline,standard,pipelined}.json
├── batch-otel-gocache-ipc-otel-gocache-ipc-otel.csv / -pipelined.csv / -memory.txt
├── batch-otel-gocache-ipc-otel-gocache-ipc-otel-telemetry-{baseline,standard,pipelined}.json
└── BENCHMARK_SUMMARY.md
```

---

*Generated from benchmark runs on 2026-07-01. Branch: perf/telemetry-tmpfs (Phase 9 — Metrics Cleanup + Slot Pool Growth).*
