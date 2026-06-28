# GoCache Benchmark Summary: Telemetry tmpfs Shared-Memory IPC (ADR-0037)

**Date:** 2026-06-28  
**Branch:** `perf/telemetry-tmpfs`  
**Commit:** `448effa67061ebe0150bd7c5f309b873f1657b2d`  
**Label:** `tmpfs-full`

---

## 1. Executive Summary

The full Valkey / GoCache core / GoCache IPC / GoCache IPC pprof+benchstats / GoCache IPC+OTel benchmark matrix completed with the requested 100K-op, 50-client, 10-deep pipeline configuration. This replaces the older 3-run OTel-only summary with the full matrix data.

- **Valkey standard mode:** **114,806 RPS** reference average across all 15 `valkey-benchmark --csv` rows.
- **GoCache core standard mode:** **100,691 RPS**, **87.7% of Valkey**.
- **GoCache IPC standard mode:** **106,818 RPS**, **93.0% of Valkey**.
- **GoCache IPC pprof/stats standard mode:** **108,909 RPS**, **94.9% of Valkey**.
- **GoCache IPC+OTel standard mode:** **102,137 RPS**, **89.0% of Valkey**.
- **Valkey pipelined mode:** **924,080 RPS** reference average.
- **GoCache core pipelined mode:** **559,938 RPS**, **60.6% of Valkey**.
- **GoCache IPC pipelined mode:** **761,684 RPS**, **82.4% of Valkey**.
- **GoCache IPC pprof/stats pipelined mode:** **737,549 RPS**, **79.8% of Valkey**.
- **GoCache IPC+OTel pipelined mode:** **728,866 RPS**, **78.9% of Valkey**.

Compared with the June 9 full matrix, the tmpfs shared-memory telemetry path materially improves pipelined IPC throughput: clean IPC pipelined throughput rose from **556,655** to **761,684 RPS** (**+36.8%**) and IPC+OTel pipelined throughput rose from **484,324** to **728,866 RPS** (**+50.5%**). Standard-mode IPC remains essentially flat vs June 9, while standard core is lower in this run.

Benchstats show the expected allocation reduction in the pprof/stats run: standard heap allocation objects fell from **99.0/eval** on June 9 to **28.5/eval**, and pipelined heap allocation objects fell from **155.1/eval** to **50.9/eval**. The pprof/stats pipelined run still reports **21,677 skipped operation-tracker allocations** and only **278,323** telemetry start/finish events out of **1,500,000** evaluations; throughput numbers are benchmark-measured command throughput, not telemetry counter-derived throughput.

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
| Valkey image | `valkey/valkey:8` |
| GoCache image | `gocache-bench:local` / `gocache-bench:local-ipc` |

### 2.2 Configurations Tested

| Configuration | Target | Label stem | Notes |
|---|---|---|---|
| Valkey | `valkey` | `tmpfs-full-valkey` | Reference server |
| GoCache core | `gocache` | `tmpfs-full-gocache` | No IPC plugins |
| GoCache IPC | `gocache-ipc` | `tmpfs-full-gocache-ipc` | Prometheus IPC plugin, full telemetry event mode |
| GoCache IPC pprof/stats | `gocache-ipc` | `tmpfs-full-pprof-gocache-ipc` | `BENCH_PPROF=1`, `BENCH_STATS=1`; profiles/benchstats captured |
| GoCache IPC+OTel | `gocache-ipc-otel` | `tmpfs-full-otel-gocache-ipc-otel` | Prometheus + instrumentation + OpenTelemetry Collector |

### 2.3 Data Sources

All throughput and latency numbers below are read from the CSV files in:

```text
bench/task-results/2026-06-28-01-perf-telemetry-tmpfs/
```

The older `tmpfs-otel-gocache-ipc-otel*.csv` repeat files are retained as raw data, but the primary matrix summary uses the `tmpfs-full-*` files for all five configurations.

---

## 3. Results

### 3.1 Throughput & Latency Summary

Average across all 15 emitted `valkey-benchmark --csv` rows.

| Configuration | Mode | Avg RPS | Avg Latency | Avg P99 | % of Valkey |
|---|---:|---:|---:|---:|---:|
| **Valkey** | Standard | **114,806** | **0.231ms** | **0.361ms** | **100.0%** |
| GoCache core | Standard | 100,691 | 0.274ms | 0.739ms | 87.7% |
| GoCache IPC | Standard | 106,818 | 0.249ms | 0.422ms | 93.0% |
| GoCache IPC pprof/stats | Standard | 108,909 | 0.245ms | 0.470ms | 94.9% |
| GoCache IPC+OTel | Standard | 102,137 | 0.261ms | 0.534ms | 89.0% |
| **Valkey** | Pipelined | **924,080** | **0.497ms** | **0.896ms** | **100.0%** |
| GoCache core | Pipelined | 559,938 | 1.068ms | 4.287ms | 60.6% |
| GoCache IPC | Pipelined | 761,684 | 0.739ms | 3.244ms | 82.4% |
| GoCache IPC pprof/stats | Pipelined | 737,549 | 0.757ms | 3.274ms | 79.8% |
| GoCache IPC+OTel | Pipelined | 728,866 | 0.775ms | 3.363ms | 78.9% |

### 3.2 Per-Command Valkey Comparison

#### 3.2.1 RPS by command, configuration, and mode

| Test | Valkey Std | Valkey Pipe | Core Std | Core Pipe | IPC Std | IPC Pipe | IPC pprof Std | IPC pprof Pipe | IPC+OTel Std | IPC+OTel Pipe |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| PING_INLINE | 118,906 | 1,098,901 | 99,010 | 990,099 | 106,157 | 980,392 | 108,342 | 877,193 | 105,374 | 813,008 |
| PING_MBULK | 118,343 | 1,111,111 | 93,545 | 990,099 | 112,613 | 1,041,667 | 115,741 | 990,099 | 100,200 | 900,901 |
| SET | 121,507 | 952,381 | 106,383 | 632,911 | 110,988 | 943,396 | 111,359 | 961,538 | 99,206 | 925,926 |
| GET | 115,075 | 1,086,956 | 101,317 | 775,194 | 96,246 | 990,099 | 112,233 | 970,874 | 104,712 | 1,041,667 |
| INCR | 118,483 | 1,075,269 | 101,937 | 613,497 | 110,254 | 892,857 | 112,613 | 925,926 | 105,932 | 925,926 |
| LPUSH | 114,679 | 1,136,364 | 97,466 | 534,759 | 112,740 | 793,651 | 110,132 | 746,269 | 99,800 | 729,927 |
| RPUSH | 115,607 | 1,162,791 | 101,937 | 460,830 | 111,359 | 746,269 | 110,742 | 662,252 | 101,317 | 704,225 |
| LPOP | 121,212 | 1,063,830 | 102,145 | 505,050 | 106,270 | 854,701 | 110,742 | 806,452 | 104,058 | 806,452 |
| RPOP | 120,919 | 862,069 | 106,157 | 512,821 | 113,122 | 800,000 | 110,865 | 813,008 | 95,602 | 793,651 |
| SADD | 108,108 | 1,030,928 | 107,411 | 495,050 | 110,497 | 757,576 | 108,342 | 704,225 | 105,708 | 719,424 |
| HSET | 118,064 | 1,020,408 | 103,520 | 469,484 | 108,460 | 724,638 | 112,233 | 684,932 | 107,643 | 666,667 |
| SPOP | 113,636 | 628,931 | 103,199 | 526,316 | 114,548 | 729,927 | 112,613 | 735,294 | 109,409 | 724,638 |
| LPUSH (needed to benchmark LRANGE) | 120,482 | 1,075,269 | 109,290 | 555,556 | 109,890 | 751,880 | 113,122 | 763,359 | 109,051 | 775,194 |
| LRANGE_100 (first 100 elements) | 80,515 | 208,768 | 73,855 | 165,289 | 71,480 | 188,324 | 76,687 | 184,843 | 75,586 | 186,567 |
| MSET (10 keys) | 116,550 | 347,222 | 103,199 | 172,117 | 107,643 | 229,885 | 107,875 | 236,967 | 108,460 | 218,818 |

#### 3.2.2 Percent of Valkey by command

| Test | Core Std | Core Pipe | IPC Std | IPC Pipe | IPC pprof Std | IPC pprof Pipe | IPC+OTel Std | IPC+OTel Pipe |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| PING_INLINE | 83.3% | 90.1% | 89.3% | 89.2% | 91.1% | 79.8% | 88.6% | 74.0% |
| PING_MBULK | 79.0% | 89.1% | 95.2% | 93.8% | 97.8% | 89.1% | 84.7% | 81.1% |
| SET | 87.6% | 66.5% | 91.3% | 99.1% | 91.6% | 101.0% | 81.6% | 97.2% |
| GET | 88.0% | 71.3% | 83.6% | 91.1% | 97.5% | 89.3% | 91.0% | 95.8% |
| INCR | 86.0% | 57.1% | 93.1% | 83.0% | 95.0% | 86.1% | 89.4% | 86.1% |
| LPUSH | 85.0% | 47.1% | 98.3% | 69.8% | 96.0% | 65.7% | 87.0% | 64.2% |
| RPUSH | 88.2% | 39.6% | 96.3% | 64.2% | 95.8% | 57.0% | 87.6% | 60.6% |
| LPOP | 84.3% | 47.5% | 87.7% | 80.3% | 91.4% | 75.8% | 85.8% | 75.8% |
| RPOP | 87.8% | 59.5% | 93.6% | 92.8% | 91.7% | 94.3% | 79.1% | 92.1% |
| SADD | 99.4% | 48.0% | 102.2% | 73.5% | 100.2% | 68.3% | 97.8% | 69.8% |
| HSET | 87.7% | 46.0% | 91.9% | 71.0% | 95.1% | 67.1% | 91.2% | 65.3% |
| SPOP | 90.8% | 83.7% | 100.8% | 116.1% | 99.1% | 116.9% | 96.3% | 115.2% |
| LPUSH (needed to benchmark LRANGE) | 90.7% | 51.7% | 91.2% | 69.9% | 93.9% | 71.0% | 90.5% | 72.1% |
| LRANGE_100 (first 100 elements) | 91.7% | 79.2% | 88.8% | 90.2% | 95.2% | 88.5% | 93.9% | 89.4% |
| MSET (10 keys) | 88.5% | 49.6% | 92.4% | 66.2% | 92.6% | 68.2% | 93.1% | 63.0% |

### 3.3 Memory Usage (RSS)

Values are decimal MB derived from each `*-memory.txt` file.

| Configuration | Baseline | Post-Standard | Final | Delta |
|---|---:|---:|---:|---:|
| Valkey | 16.1 MB | 32.0 MB | 34.9 MB | +18.8 MB |
| GoCache core | 59.6 MB | 247.5 MB | 291.5 MB | +231.9 MB |
| GoCache IPC | 63.6 MB | 205.7 MB | 239.7 MB | +176.1 MB |
| GoCache IPC pprof/stats | 67.9 MB | 205.9 MB | 237.7 MB | +169.8 MB |
| GoCache IPC+OTel | 67.5 MB | 212.0 MB | 258.7 MB | +191.1 MB |

#### OTel Collector Memory (IPC+OTel variant)

| Metric | Value |
|---|---:|
| Baseline RSS | 41.5 MB |
| Post-Standard RSS | 41.5 MB |
| Final RSS | 41.5 MB |
| Delta | 0 MB (stable) |

### 3.4 Runtime Metrics (benchstats)

Complete benchstats were captured for `tmpfs-full-pprof-gocache-ipc`, the only variant run with `BENCH_STATS=1`.

#### Standard Mode

| Metric | Value | Per-Evaluation |
|---|---:|---:|
| `enabled` | true | — |
| `manager.event_dropped` | 0 | 0 |
| `manager.event_enqueue_attempts` | 0 | 0 |
| `manager.event_received` | 0 | 0 |
| `manager.projection_builds` | 0 | 0 |
| `operation_tracker.dropped_completed` | 0 | 0 |
| `operation_tracker.dropped_records` | 0 | 0 |
| `operation_tracker.skipped_operations` | 0 | 0 |
| `pipeline.evaluations` | 1,500,000 | — |
| `pipeline.event.operation_completed` | 1,500,000 | 1.000 |
| `pipeline.event.operation_started` | 1,500,000 | 1.000 |
| `runtime.gc.heap.allocs.bytes` | 3,184,830,856 | 2,123 |
| `runtime.gc.heap.allocs.objects` | 42,810,876 | 28.5 |
| `runtime.gc.heap.objects.objects` | 815,973 | 0.5 |
| `runtime.memory.classes.heap.objects.bytes` | 101,205,144 | 67.5 |
| `runtime.memory.classes.total.bytes` | 207,534,376 | 138.4 |
| `runtime.sched.goroutines.goroutines` | 30 | — |
| `runtime.sync.mutex.wait.total.seconds` | 2.54s | 1.7 µs |

#### Pipelined Mode

| Metric | Value | Per-Evaluation |
|---|---:|---:|
| `enabled` | true | — |
| `manager.event_dropped` | 0 | 0 |
| `manager.event_enqueue_attempts` | 0 | 0 |
| `manager.event_received` | 0 | 0 |
| `manager.projection_builds` | 0 | 0 |
| `operation_tracker.dropped_completed` | 0 | 0 |
| `operation_tracker.dropped_records` | 0 | 0 |
| `operation_tracker.skipped_operations` | 21,677 | 0.014 |
| `pipeline.evaluations` | 1,500,000 | — |
| `pipeline.event.operation_completed` | 278,323 | 0.186 |
| `pipeline.event.operation_started` | 278,323 | 0.186 |
| `runtime.gc.heap.allocs.bytes` | 5,880,034,392 | 3,920 |
| `runtime.gc.heap.allocs.objects` | 76,371,183 | 50.9 |
| `runtime.gc.heap.objects.objects` | 1,276,675 | 0.9 |
| `runtime.memory.classes.heap.objects.bytes` | 132,089,744 | 88.1 |
| `runtime.memory.classes.total.bytes` | 268,298,552 | 178.9 |
| `runtime.sched.goroutines.goroutines` | 30 | — |
| `runtime.sync.mutex.wait.total.seconds` | 70.26s | 46.8 µs |

#### Standard vs Pipelined Benchstats Comparison

| Metric | Standard | Pipelined | Delta |
|---|---:|---:|---:|
| Evaluations | 1,500,000 | 1,500,000 | 0 |
| Operation started/completed events | 1,500,000 | 278,323 | -81.4% |
| Operation events / eval | 1.000 | 0.186 | -0.814 |
| Skipped operation-tracker allocations | 0 | 21,677 | +21,677 |
| Heap alloc bytes / eval | 2,123 | 3,920 | +84.6% |
| Heap alloc objects / eval | 28.5 | 50.9 | +78.4% |
| Mutex wait / eval | 1.7 µs | 46.8 µs | +45.1 µs |
| Total runtime memory classes / eval | 138.4 bytes | 178.9 bytes | +29.3% |

**Notes:**

1. The pprof/stats run is a Prometheus-only IPC run, so `manager.event_*` metrics remain zero. Instrumentation/OTel runs are measured by their CSV throughput and memory files, not this benchstats capture.
2. Pipelined telemetry event counters are lower than evaluations because operation-tracker allocation can be skipped under high concurrent pipeline pressure. The benchmark CSV files remain the throughput source of truth.

### 3.5 Pprof vs Clean IPC Comparison

Comparing IPC clean build vs IPC pprof/stats build quantifies profiling/benchstats overhead in this run.

| Metric | IPC Clean | IPC Pprof/Stats | Diff |
|---|---:|---:|---:|
| Standard RPS | 106,818 | 108,909 | +2.0% |
| Pipelined RPS | 761,684 | 737,549 | -3.2% |
| Standard latency | 0.249ms | 0.245ms | -1.6% |
| Pipelined latency | 0.739ms | 0.757ms | +2.4% |
| Standard P99 | 0.422ms | 0.470ms | +11.4% |
| Pipelined P99 | 3.244ms | 3.274ms | +0.9% |
| Baseline RSS | 63.6 MB | 67.9 MB | +6.8% |
| Final RSS | 239.7 MB | 237.7 MB | -0.8% |

---

## 4. Comparison with June 9 Full Matrix

June 9 values are from `bench/task-results/2026-06-09-01-perf-connection-sharding-telemetry-drain-improvements/BENCHMARK_SUMMARY.md`.

### 4.1 Throughput Comparison

| Metric | June 9 | June 28 tmpfs-full | Delta |
|---|---:|---:|---:|
| Valkey standard avg RPS | 117,425 | 114,806 | -2.2% |
| Valkey pipelined avg RPS | 960,072 | 924,080 | -3.7% |
| Core standard avg RPS | 108,785 | 100,691 | -7.4% |
| Core standard % of Valkey | 92.6% | 87.7% | -4.9 pp |
| Core pipelined avg RPS | 556,645 | 559,938 | +0.6% |
| Core pipelined % of Valkey | 58.0% | 60.6% | +2.6 pp |
| IPC standard avg RPS | 107,618 | 106,818 | -0.7% |
| IPC standard % of Valkey | 91.6% | 93.0% | +1.4 pp |
| IPC pipelined avg RPS | 556,655 | 761,684 | +36.8% |
| IPC pipelined % of Valkey | 58.0% | 82.4% | +24.4 pp |
| IPC pprof/stats standard avg RPS | 99,583 | 108,909 | +9.4% |
| IPC pprof/stats pipelined avg RPS | 533,104 | 737,549 | +38.4% |
| IPC+OTel standard avg RPS | 106,053 | 102,137 | -3.7% |
| IPC+OTel standard % of Valkey | 90.3% | 89.0% | -1.3 pp |
| IPC+OTel pipelined avg RPS | 484,324 | 728,866 | +50.5% |
| IPC+OTel pipelined % of Valkey | 50.4% | 78.9% | +28.5 pp |

### 4.2 Benchstats Comparison (pprof-enabled IPC runs)

| Metric | June 9 Standard | June 28 Standard | June 9 Pipelined | June 28 Pipelined |
|---|---:|---:|---:|---:|
| Mutex wait | 5.96s | 2.54s | 109.00s | 70.26s |
| Mutex wait / eval | 4.0 µs | 1.7 µs | 72.7 µs | 46.8 µs |
| Operations / eval | 1.000 | 1.000 | 0.474 | 0.186 |
| Skipped operation-tracker allocations | 0 | 0 | 0 reported | 21,677 |
| Heap allocs / eval | 99.0 | 28.5 | 155.1 | 50.9 |
| Bytes / eval | 5,694 | 2,123 | 9,230 | 3,920 |

The tmpfs shared-memory path substantially reduces runtime allocation and mutex-wait metrics in the pprof/stats run. Pipelined telemetry event coverage is lower than June 9's reported operation-event ratio, but the clean IPC and IPC+OTel CSV throughput is much higher.

### 4.3 Memory Comparison vs June 9

| Configuration | June 9 Final RSS | June 28 Final RSS | Delta |
|---|---:|---:|---:|
| Valkey | 35.1 MB | 34.9 MB | -0.2 MB |
| GoCache core | 252.4 MB | 291.5 MB | +39.1 MB |
| GoCache IPC | 271.0 MB | 239.7 MB | -31.3 MB |
| GoCache IPC pprof/stats | 280.9 MB | 237.7 MB | -43.2 MB |
| GoCache IPC+OTel target | 265.5 MB | 258.7 MB | -6.8 MB |
| OTel Collector | 209.5 MB | 41.5 MB | -168.0 MB |

---

## 5. Key Findings

1. **The tmpfs IPC path closes most of the pipelined gap to Valkey for IPC modes.** Clean IPC pipelined mode reaches **82.4% of Valkey**, up from **58.0%** on June 9.
2. **IPC+OTel pipelined throughput is the main improvement.** It rises from **484,324** to **728,866 RPS** (**+50.5%**) and from **50.4%** to **78.9%** of Valkey.
3. **Standard-mode throughput is mixed.** Clean IPC standard is nearly unchanged vs June 9, IPC+OTel standard is slightly lower, and core standard is lower in this run.
4. **Allocation pressure is much lower in benchstats.** Standard alloc objects/eval fell from **99.0** to **28.5**; pipelined fell from **155.1** to **50.9**.
5. **Pipelined operation-tracker skip accounting is now visible.** The pprof/stats pipelined benchstats report **21,677** skipped operation-tracker allocations and **0.186** operation events/eval.
6. **Memory improved for IPC variants vs June 9.** Clean IPC final RSS is **239.7 MB** vs **271.0 MB** on June 9; pprof/stats final RSS is **237.7 MB** vs **280.9 MB**.
7. **Core memory is higher than June 9.** Core final RSS is **291.5 MB**, up **39.1 MB** from the June 9 summary.

---

## 6. Issues / Notes

- The primary full matrix is the `tmpfs-full-*` dataset. The older `tmpfs-otel-*` repeat files remain in the directory and were read, but they are not the source for the five-configuration matrix tables.
- `tmpfs-otel-gocache-ipc-otel.csv` / `tmpfs-otel-gocache-ipc-otel-pipelined.csv` correspond to the earlier OTel-only repeat (`tmpfs-otel-r2`, commit `04360a54e99fd38a2bb1abdc348f6c37b931f0a7`) and are superseded for this summary by `tmpfs-full-otel-gocache-ipc-otel*.csv`.
- Pipelined telemetry event counts in benchstats do not equal command evaluations. CSV RPS is measured by `valkey-benchmark` and remains the authoritative throughput metric.

---

## 7. Raw Data Files

Primary result directory:

```text
bench/task-results/2026-06-28-01-perf-telemetry-tmpfs/
```

Key files:

- `tmpfs-full-valkey.csv`
- `tmpfs-full-valkey-pipelined.csv`
- `tmpfs-full-valkey-memory.txt`
- `tmpfs-full-gocache.csv`
- `tmpfs-full-gocache-pipelined.csv`
- `tmpfs-full-gocache-memory.txt`
- `tmpfs-full-gocache-ipc.csv`
- `tmpfs-full-gocache-ipc-pipelined.csv`
- `tmpfs-full-gocache-ipc-memory.txt`
- `tmpfs-full-gocache-ipc-config.yaml`
- `tmpfs-full-pprof-gocache-ipc.csv`
- `tmpfs-full-pprof-gocache-ipc-pipelined.csv`
- `tmpfs-full-pprof-gocache-ipc-memory.txt`
- `tmpfs-full-pprof-gocache-ipc-benchstats-baseline.json`
- `tmpfs-full-pprof-gocache-ipc-benchstats-standard.json`
- `tmpfs-full-pprof-gocache-ipc-benchstats-pipelined.json`
- `tmpfs-full-otel-gocache-ipc-otel.csv`
- `tmpfs-full-otel-gocache-ipc-otel-pipelined.csv`
- `tmpfs-full-otel-gocache-ipc-otel-memory.txt`
- `tmpfs-full-otel-gocache-ipc-otel-config.yaml`
- `tmpfs-full-otel-gocache-ipc-otel-otel-collector.yaml`
- `tmpfs-otel-gocache-ipc-otel.csv` (older OTel-only repeat)
- `tmpfs-otel-gocache-ipc-otel-pipelined.csv` (older OTel-only repeat)
- `tmpfs-otel-gocache-ipc-otel-memory.txt` (older OTel-only repeat)

---

*Generated from benchmark runs on 2026-06-28. Branch: perf/telemetry-tmpfs (ADR-0037).*
