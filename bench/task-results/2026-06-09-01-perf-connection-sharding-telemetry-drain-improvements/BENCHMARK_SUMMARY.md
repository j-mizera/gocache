# GoCache Benchmark Summary: Connection Sharding / Telemetry Drain Improvements

**Date:** 2026-06-09  
**Branch:** `perf/connection-sharding-telemetry-drain-improvements`  
**Commit:** `545b5f61b28b26ec46f89a0a96b85c1c6e722a71`  
**Label:** `magazine-bench`

---

## 1. Executive Summary

The full Valkey / GoCache core / GoCache IPC benchmark matrix completed with the requested 100K-op, 50-client configuration. On this branch:

- **GoCache core standard mode:** **108,785 RPS**, **92.6% of current Valkey**.
- **GoCache IPC standard mode:** **107,618 RPS**, **91.6% of current Valkey**.
- **GoCache IPC+OTel standard mode:** **106,053 RPS**, **90.3% of current Valkey**.
- **GoCache core pipelined mode:** **556,645 RPS**, **58.0% of current Valkey**.
- **GoCache IPC pipelined mode:** **556,655 RPS**, **58.0% of current Valkey**.
- **GoCache IPC+OTel pipelined mode:** **484,324 RPS**, **50.4% of current Valkey**.

Compared with the previous `benchstats-stripped-20260609` summary, current core standard absolute throughput is slightly lower (**-1.5%**), but current Valkey was also lower on this run; relative standard throughput improved from **90.1%** to **92.6%** of Valkey. Core pipelined throughput improved from **544,186** to **556,645 RPS** (**+2.3%**) and from **55.7%** to **58.0%** of Valkey.

The pprof mutex profile shows the expected telemetry contention improvement: `SlotOperationTrackerManager.FinishOperation` fell from the previous **0.69s / 7.9%** mutex sample to **0.283s / 0.29%** in the full current profile. The dominant mutex bottleneck is now overwhelmingly shard locking/unlocking, especially multi-shard `MSET` / pipelined batch paths.

---

## 2. Methodology

### 2.1 Test Configuration

| Parameter | Value |
|---|---:|
| Operations per test (`BENCH_N`) | 100,000 |
| Clients (`BENCH_CLIENTS`) | 50 |
| Keyspace (`BENCH_KEYSPACE`) | 100,000 |
| Pipeline depth (`BENCH_PIPELINE`) | 10 |
| Target CPUs | `0-3` |
| Client CPUs | `4-7` |
| Container memory limit | `2g` |
| GoCache max memory | 1024 MB |
| Valkey image | `valkey/valkey:8` |

### 2.2 Configurations Tested

| Configuration | Target | Label stem | Notes |
|---|---|---|---|
| Valkey | `valkey` | `magazine-bench-valkey` | Reference server |
| GoCache core | `gocache` | `magazine-bench-gocache` | No IPC plugins |
| GoCache IPC | `gocache-ipc` | `magazine-bench-gocache-ipc` | Prometheus IPC plugin |
| GoCache IPC pprof/stats | `gocache-ipc` | `magazine-bench-pprof-gocache-ipc` | `BENCH_PPROF=1`, `BENCH_STATS=1`; profiles captured during/after workload |
| GoCache IPC+OTel | `gocache-ipc-otel` | `magazine-bench-otel-gocache-ipc-otel` | Prometheus + instrumentation + OpenTelemetry Collector |

### 2.3 Commands Run

```bash
export BENCH_N=100000
export BENCH_CLIENTS=50
export BENCH_KEYSPACE=100000
export BENCH_PIPELINE=10
export BENCH_TARGET_CPUS=0-3
export BENCH_CLIENT_CPUS=4-7
export BENCH_MEM_LIMIT=2g
export BENCH_GOCACHE_MAX_MEMORY_MB=1024
export REBUILD=1
# Basic matrix (valkey + core + IPC prometheus)
bench/redis-benchmark/run-matrix.sh magazine-bench

# IPC with pprof and benchstats (for profiles)
export BENCH_PPROF=1
export BENCH_STATS=1
bench/redis-benchmark/run-ipc.sh magazine-bench-pprof --target gocache-ipc

# IPC with OpenTelemetry (REQUIRED)
bench/redis-benchmark/run-ipc.sh magazine-bench-otel --target gocache-ipc-otel
```

For final post-workload profile capture, a temporary copy of `run-ipc.sh` was used with a short hold before container cleanup so `heap`, `goroutine`, `block`, and `mutex` profiles could be fetched after the pipelined suite completed. The final `magazine-bench-pprof-*` CSV/benchstats files are from that held rerun.

---

## 3. Results

### 3.1 Throughput & Latency Summary

Average across all 15 emitted `valkey-benchmark --csv` rows.

| Configuration | Mode | Avg RPS | Avg Latency | Avg P99 | % of Valkey |
|---|---:|---:|---:|---:|---:|
| **Valkey** | Standard | **117,425** | **0.225ms** | **0.348ms** | **100.0%** |
| GoCache core | Standard | 108,785 | 0.251ms | 0.591ms | 92.6% |
| GoCache IPC | Standard | 107,618 | 0.254ms | 0.649ms | 91.6% |
| GoCache IPC pprof/stats | Standard | 99,583 | 0.276ms | 0.688ms | 84.8% |
| GoCache IPC+OTel | Standard | 106,053 | 0.257ms | 0.556ms | 90.3% |
| **Valkey** | Pipelined | **960,072** | **0.489ms** | **0.900ms** | **100.0%** |
| GoCache core | Pipelined | 556,645 | 1.079ms | 4.515ms | 58.0% |
| GoCache IPC | Pipelined | 556,655 | 1.085ms | 4.381ms | 58.0% |
| GoCache IPC pprof/stats | Pipelined | 533,104 | 1.121ms | 4.775ms | 55.5% |
| GoCache IPC+OTel | Pipelined | 484,324 | 1.206ms | 4.847ms | 50.4% |

### 3.2 Key Operations Detail

| Operation | Config | Standard RPS | Pipelined RPS | Standard Lat | Pipelined Lat |
|---|---|---:|---:|---:|---:|
| SET | Valkey | 122,399 | 952,381 | 0.213ms | 0.470ms |
| SET | GoCache core | 112,740 | 662,252 | 0.235ms | 0.729ms |
| SET | GoCache IPC | 106,952 | 671,141 | 0.248ms | 0.733ms |
| GET | Valkey | 118,906 | 1,123,596 | 0.217ms | 0.373ms |
| GET | GoCache core | 110,742 | 806,452 | 0.239ms | 0.553ms |
| GET | GoCache IPC | 109,051 | 775,194 | 0.245ms | 0.588ms |
| INCR | Valkey | 123,305 | 1,063,830 | 0.212ms | 0.409ms |
| INCR | GoCache core | 110,619 | 613,497 | 0.239ms | 0.802ms |
| INCR | GoCache IPC | 112,486 | 609,756 | 0.235ms | 0.806ms |
| LPUSH | Valkey | 118,765 | 1,123,596 | 0.220ms | 0.380ms |
| LPUSH | GoCache core | 114,286 | 505,050 | 0.233ms | 0.980ms |
| LPUSH | GoCache IPC | 111,235 | 512,821 | 0.248ms | 0.965ms |
| LRANGE_100 | Valkey | 81,699 | 242,131 | 0.313ms | 1.055ms |
| LRANGE_100 | GoCache core | 76,220 | 160,256 | 0.375ms | 2.689ms |
| LRANGE_100 | GoCache IPC | 73,855 | 158,730 | 0.384ms | 2.666ms |
| MSET | Valkey | 120,337 | 369,004 | 0.233ms | 1.250ms |
| MSET | GoCache core | 104,822 | 177,305 | 0.301ms | 2.804ms |
| MSET | GoCache IPC | 108,578 | 183,150 | 0.283ms | 2.715ms |
| SET | GoCache IPC+OTel | 113,507 | 448,430 | 0.235ms | 1.095ms |
| GET | GoCache IPC+OTel | 112,108 | 675,676 | 0.239ms | 0.656ms |
| INCR | GoCache IPC+OTel | 98,135 | 490,196 | 0.286ms | 0.993ms |
| LPUSH | GoCache IPC+OTel | 108,578 | 420,168 | 0.245ms | 1.171ms |
| LRANGE_100 | GoCache IPC+OTel | 71,891 | 152,439 | 0.401ms | 2.811ms |
| MSET | GoCache IPC+OTel | 104,603 | 173,611 | 0.308ms | 2.865ms |

### 3.3 Memory Usage (RSS)

| Configuration | Baseline | Post-Standard | Final | Delta |
|---|---|---:|---:|---:|
| Valkey | 16.7 MB | 32.2 MB | 35.1 MB | +18.4 MB |
| GoCache core | 48.5 MB | 212.3 MB | 252.4 MB | +203.9 MB |
| GoCache IPC | 53.5 MB | 221.6 MB | 271.0 MB | +217.5 MB |
| GoCache IPC pprof/stats | 56.5 MB | 220.5 MB | 280.9 MB | +224.4 MB |
| GoCache IPC+OTel | 53.8 MB | 219.9 MB | 265.5 MB | +211.6 MB |

#### OTel Collector Memory (IPC+OTel variant)

| Metric | Value |
|---|---:|
| Baseline RSS | 209.5 MB |
| Post-Standard RSS | 209.5 MB |
| Final RSS | 209.5 MB |
| Delta | 0 MB (stable) |

The OTel Collector container memory remained stable at ~200 MB throughout the benchmark, indicating no memory leak under load.

### 3.4 Runtime Metrics (benchstats)

Complete benchstats from `magazine-bench-pprof-gocache-ipc` (the only variant run with `BENCH_STATS=1`). **All metrics from the JSON files are displayed below.**

#### Standard Mode

| Metric | Value | Per-Evaluation |
|---|---|---:|
| `enabled` | true | — |
| `manager.event_enqueue_attempts` | 0 | 0 |
| `manager.event_received` | 0 | 0 |
| `manager.projection_builds` | 0 | 0 |
| `operation_tracker.dropped_completed` | 0 | 0 |
| `operation_tracker.dropped_records` | 0 | 0 |
| `operation_tracker.skipped_operations` | 0 | 0 |
| `pipeline.evaluations` | 1,500,000 | — |
| `pipeline.event.operation_completed` | 1,499,978 | 1.000 |
| `pipeline.event.operation_started` | 1,499,978 | 1.000 |
| `runtime.gc.heap.allocs.bytes` | 8,540,847,936 | 5,694 |
| `runtime.gc.heap.allocs.objects` | 148,496,956 | 99.0 |
| `runtime.gc.heap.objects.objects` | 1,534,904 | 1.0 |
| `runtime.memory.classes.heap.objects.bytes` | 134,184,376 | 89.5 |
| `runtime.memory.classes.total.bytes` | 220,973,368 | 147.3 |
| `runtime.sched.goroutines.goroutines` | 23 | — |
| `runtime.sync.mutex.wait.total.seconds` | 5.96s | 4.0 µs |

#### Pipelined Mode

| Metric | Value | Per-Evaluation |
|---|---|---:|
| `enabled` | true | — |
| `manager.event_enqueue_attempts` | 0 | 0 |
| `manager.event_received` | 0 | 0 |
| `manager.projection_builds` | 0 | 0 |
| `operation_tracker.dropped_completed` | 0 | 0 |
| `operation_tracker.dropped_records` | 0 | 0 |
| `operation_tracker.skipped_operations` | 0 | 0 |
| `pipeline.evaluations` | 1,500,000 | — |
| `pipeline.event.operation_completed` | 710,257 | 0.474 |
| `pipeline.event.operation_started` | 710,257 | 0.474 |
| `runtime.gc.heap.allocs.bytes` | 13,844,450,480 | 9,230 |
| `runtime.gc.heap.allocs.objects` | 232,644,870 | 155.1 |
| `runtime.gc.heap.objects.objects` | 1,924,423 | 1.3 |
| `runtime.memory.classes.heap.objects.bytes` | 154,061,112 | 102.7 |
| `runtime.memory.classes.total.bytes` | 281,274,680 | 187.5 |
| `runtime.sched.goroutines.goroutines` | 23 | — |
| `runtime.sync.mutex.wait.total.seconds` | 109.00s | 72.7 µs |

**Notes:**

1. **Event metrics are zero** because this is a Prometheus-only IPC run (no `instrumentation` plugin). The `manager.event_*` metrics only populate when the `instrumentation` plugin is active, which requires the OTel variant.

2. **Pipelined operations per evaluation (~0.47) is expected.** In pipelined mode, commands are batched (10 per pipeline) and processed as shared batches. The 0.47-0.52 ops/eval range is normal for pipelined workloads — it reflects batch sharing overhead, not dropped operations. Total throughput is still measured via RPS from the benchmark tool.

### 3.5 Pprof vs Clean Build Comparison

Comparing IPC clean build (no profiling) vs IPC pprof build (with profiling enabled) to quantify profiling overhead:

| Metric | IPC Clean | IPC Pprof | Diff |
|---|---|---:|---:|
| Standard RPS | 107,618 | 99,583 | -7.5% |
| Pipelined RPS | 556,655 | 533,104 | -4.2% |
| Standard Latency | 0.254ms | 0.276ms | +8.7% |
| Pipelined Latency | 1.085ms | 1.121ms | +3.3% |
| Standard P99 | 0.649ms | 0.688ms | +6.0% |
| Pipelined P99 | 4.381ms | 4.775ms | +9.0% |
| Baseline RSS | 53.5 MB | 56.5 MB | +5.6% |
| Final RSS | 271.0 MB | 280.9 MB | +3.7% |

The pprof-enabled build shows a measurable throughput reduction (~4-8%) and slightly higher latency, which is expected due to profiling overhead. This is why clean builds are used for primary throughput comparisons, while pprof builds are used only for profile capture and benchstats collection.

---

## 4. Mutex Contention Analysis

Profiles were captured under:

```text
bench/task-results/2026-06-09-01-perf-connection-sharding-telemetry-drain-improvements/magazine-bench-pprof-gocache-ipc-profiles/
```

Key generated reports:

- `cpu-held-10s.prof` / `cpu-held-10s.top.txt`
- `mutex-final.prof` / `mutex-final.top.txt` / `mutex-final.fulltop.txt`
- `block-final.prof` / `block-final.top.txt` / `block-final.fulltop.txt`
- `heap-final.prof` / `heap-final.alloc_space.txt`

### 4.1 Mutex Profile: Full Workload

`mutex-final.prof` total sampled delay: **96.83s**.

| Location | Cumulative | % of Total | Notes |
|---|---:|---:|---|
| `sync.(*Mutex).Unlock` | 95.96s | 99.10% | Dominant unlock wakeup path |
| `cache.(*Shard).Unlock` / `sync.(*RWMutex).Unlock` | 95.42s | 98.55% | Shard unlock dominates total mutex delay |
| `server.(*Server).runBatch` | 66.35s | 68.52% | Pipelined batch path |
| `pipeline.(*Pipeline).evaluateCore` | 29.36s | 30.32% | Standard command path cumulative |
| `command.Dispatch` | 29.08s | 30.03% | Dispatch lock path cumulative |
| `engine.(*Engine).DispatchToShards` / `HandleMset` | 27.95s | 28.87% | Multi-shard MSET pressure |
| `SlotOperationTrackerManager.UpdateConnectionContextStrings` | 0.494s | 0.51% | Connection context update |
| `SlotOperationTrackerManager.FinishOperation` | 0.283s | 0.29% | Greatly reduced vs previous baseline |

### 4.2 Block Profile

`block-final.prof` total sampled delay: **199.09s**.

| Location | Cumulative | % of Total | Notes |
|---|---:|---:|---|
| `sync.(*Mutex).Lock` | 109.56s | 55.03% | Active shard lock waiting |
| `sync.(*RWMutex).Lock` / `cache.(*Shard).Lock` | 109.04s | 54.77% | Shard lock acquisition |
| `runtime.selectgo` | 89.36s | 44.89% | Mostly background goroutines / pprof sleep / health loops |
| `server.(*Server).runBatch` | 79.05s | 39.71% | Pipelined batch path |
| `engine.(*Engine).AcquireShard` | 79.03s | 39.70% | Batch shard acquisition |
| `pipeline.evaluateCore` | 30.05s | 15.10% | Standard path cumulative |
| `HandleMset` / `Cache.LockShards` | 29.02s | 14.58% | Multi-shard MSET path |

### 4.3 CPU Profile

The 10s CPU profile captured during the workload showed:

| Function / Path | Cumulative | % of Samples | Notes |
|---|---:|---:|---|
| `server.(*Server).handleConnection` | 7.81s | 50.88% | RESP request handling |
| `internal/runtime/syscall.Syscall6` | 5.84s | 38.05% | Network I/O |
| `OperationTrackerDrainWorker.Start.func1` | 4.30s | 28.01% | Telemetry drain worker |
| `SlotOperationTrackerManager.DrainCompletedShard` | 3.87s | 25.21% | Drain worker scan/project path |
| `ConnHandle.Flush` / `bufio.Writer.Flush` | 3.86s | 25.15% | Response writes |
| `OperationTrackerDrainWorker.projectCompletedOperation` | 3.67s | 23.91% | Completed-operation projection |
| `events.Bus.Emit` | 2.02s | 13.16% | Event bus emission |
| `pipeline.evaluateCore` | 1.95s | 12.70% | Command evaluation |

### 4.4 Allocation Profile

`heap-final.alloc_space.txt` total alloc space: **12.80GB**.

| Function / Path | Cumulative | % of Alloc Space |
|---|---:|---:|
| `OperationTrackerDrainWorker.Start.func1` | 8.11GB | 63.35% |
| `SlotOperationTrackerManager.DrainCompletedShard` | 7.88GB | 61.58% |
| `OperationTrackerDrainWorker.projectCompletedOperation` | 7.38GB | 57.64% |
| `server.(*Server).handleConnection` | 4.64GB | 36.27% |
| `materializeOperationFinishedRecord` | 3.44GB | 26.88% |
| `events.Bus.Emit` / `events.ring.push` / `events.cloneEvent` | 3.42GB | 26.75% |
| `server.(*Server).runBatch` | 1.90GB | 14.88% |
| `pipeline.evaluateCore` | 1.78GB | 13.89% |

---

## 5. Comparison with `benchstats-stripped-20260609`

### 5.1 Throughput Comparison

| Metric | Previous Baseline | Current | Delta |
|---|---:|---:|---:|
| Valkey standard avg RPS | 122,462 | 117,425 | -4.1% |
| Valkey pipelined avg RPS | 976,652 | 960,072 | -1.7% |
| Core standard avg RPS | 110,393 | 108,785 | -1.5% |
| Core standard % of Valkey | 90.1% | 92.6% | +2.5 pp |
| Core pipelined avg RPS | 544,186 | 556,645 | +2.3% |
| Core pipelined % of Valkey | 55.7% | 58.0% | +2.3 pp |

Current IPC results are nearly identical to current core in pipelined mode (**556,655 vs 556,645 RPS**) and about **1.1%** below core in standard mode.

### 5.2 Benchstats Comparison (pprof-enabled core/IPC runs)

| Metric | Previous pprof2 Standard | Current Standard | Previous pprof2 Pipelined | Current Pipelined |
|---|---:|---:|---:|---:|
| Mutex wait | 5.20s | 5.96s | 100.79s | 109.00s |
| Mutex wait / eval | 3.5 µs | 4.0 µs | 67.2 µs | 72.7 µs |
| Operations / eval | 1.000 | 1.000 | 0.476 | 0.474 |
| Heap allocs / eval | 98.7 | 99.0 | 155.0 | 155.1 |
| Bytes / eval | 5,671 | 5,694 | 9,202 | 9,230 |

The runtime-level mutex wait counters are modestly higher in this pprof/stats run, but allocation rates remain nearly unchanged.

### 5.3 Slot Tracker Mutex Comparison

| Profile point | Previous | Current |
|---|---:|---:|
| `SlotOperationTrackerManager.FinishOperation` mutex sample | 0.69s / 7.9% | 0.283s / 0.29% |
| `connectionContextStore.updateStrings` / context update | 0.48s / 5.5% | 0.494s / 0.51% |
| Dominant mutex point | Shard unlock, 64.4% | Shard unlock, 98.55% |

The slot tracker magazine optimization appears to have removed `FinishOperation` as a meaningful mutex-contention hotspot. The remaining profile is almost entirely cache shard lock/unlock pressure.

---

## 6. Key Findings

1. **Relative throughput improved vs Valkey.** Core standard rose from 90.1% to 92.6% of Valkey, and core pipelined rose from 55.7% to 58.0%.
2. **Absolute core standard throughput is noise/slightly lower, but pipelined improved.** Core standard was -1.5% vs baseline; core pipelined was +2.3%.
3. **IPC overhead is very small in this Prometheus-only run.** IPC is ~1.1% below core standard and effectively equal to core pipelined.
3a. **OTel adds measurable overhead.** IPC+OTel is ~2.5% below IPC standard (90.3% vs 91.6%) and ~13% below IPC pipelined (50.4% vs 58.0%). The OTel Collector itself consumed ~200 MB RSS and was stable throughout.
4. **`FinishOperation` mutex contention is effectively resolved.** It is 0.29% of the final mutex profile, down from 7.9% previously.
5. **Shard locking is now the clear bottleneck.** Full mutex and block profiles both point to shard `RWMutex` acquisition/unlock, especially `runBatch`, `AcquireShard`, `DispatchToShards`, and `MSET`.
6. **Telemetry drain still has CPU/allocation cost.** The drain worker no longer dominates mutex time, but it is still prominent in CPU and allocation profiles.
7. **Memory growth remains much larger than Valkey.** GoCache core grew +203.9 MB and IPC grew +217.5 MB vs Valkey +18.4 MB.

---

## 7. Issues / Notes

- Docker was available and all benchmark suites completed.
- Docker emitted warnings about missing CLI plugin metadata for several optional plugins (notably `docker-buildx`), then fell back to the legacy builder successfully.
- `valkey-benchmark` emitted `WARNING: Could not fetch server CONFIG` for GoCache targets; benchmark CSVs were still produced normally.
- The first opportunistic profile-capture attempt timed out while waiting for pprof during an image rebuild; the pprof benchmark was rerun successfully, and final profiles were captured with a temporary held copy of the script.

---

## 8. Raw Data Files

Primary result directory:

```text
bench/task-results/2026-06-09-01-perf-connection-sharding-telemetry-drain-improvements/
```

Key files:

- `magazine-bench-valkey.csv`
- `magazine-bench-valkey-pipelined.csv`
- `magazine-bench-valkey-memory.txt`
- `magazine-bench-gocache.csv`
- `magazine-bench-gocache-pipelined.csv`
- `magazine-bench-gocache-memory.txt`
- `magazine-bench-gocache-ipc.csv`
- `magazine-bench-gocache-ipc-pipelined.csv`
- `magazine-bench-gocache-ipc-memory.txt`
- `magazine-bench-gocache-ipc-config.yaml`
- `magazine-bench-pprof-gocache-ipc.csv`
- `magazine-bench-pprof-gocache-ipc-pipelined.csv`
- `magazine-bench-pprof-gocache-ipc-memory.txt`
- `magazine-bench-pprof-gocache-ipc-benchstats-baseline.json`
- `magazine-bench-pprof-gocache-ipc-benchstats-standard.json`
- `magazine-bench-pprof-gocache-ipc-benchstats-pipelined.json`
- `magazine-bench-pprof-gocache-ipc-profiles/*.prof`
- `magazine-bench-pprof-gocache-ipc-profiles/*.top.txt`
- `magazine-bench-otel-gocache-ipc-otel.csv`
- `magazine-bench-otel-gocache-ipc-otel-pipelined.csv`
- `magazine-bench-otel-gocache-ipc-otel-memory.txt`
- `magazine-bench-otel-gocache-ipc-otel-config.yaml`
- `magazine-bench-otel-gocache-ipc-otel-otel-collector.yaml`

---

*Generated from benchmark runs on 2026-06-09.*
