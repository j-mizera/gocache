# GoCache Benchmark Summary: Record Arena + Drain Buffer Pool (Phase 6)

**Date:** 2026-06-30  
**Branch:** `perf/telemetry-processing`  
**Change:** RecordArena chunk-chain storage (6.1-6.7) + drain buffer pool optimization (6.8)

---

## 1. Executive Summary

Full Valkey / GoCache core / GoCache IPC / GoCache IPC pprof+stats / GoCache IPC+OTel benchmark matrix after Phase 6 RecordArena rework (replacing flat pre-allocated `segmentSize × recordsPerOperation` array with dynamic chunk-chain arena) and drain buffer pool optimization (recycling `[]TelemetryRecord` buffers across drain cycles).

- **Valkey standard:** **114,238 RPS** reference average.
- **GoCache core standard:** **112,336 RPS**, **98.3% of Valkey**.
- **GoCache IPC standard:** **103,846 RPS**, **90.9% of Valkey**.
- **GoCache IPC pprof/stats standard:** **106,395 RPS**, **93.1% of Valkey**.
- **GoCache IPC+OTel standard:** **105,728 RPS**, **92.5% of Valkey**.
- **Valkey pipelined:** **845,171 RPS** reference average.
- **GoCache core pipelined:** **681,645 RPS**, **80.7% of Valkey**.
- **GoCache IPC pipelined:** **653,714 RPS**, **77.4% of Valkey**.
- **GoCache IPC pprof/stats pipelined:** **642,330 RPS**, **76.0% of Valkey**.
- **GoCache IPC+OTel pipelined:** **647,642 RPS**, **76.6% of Valkey**.

**Standard mode achieved 100% telemetry coverage** (1,500,000/1,500,000 operations started/completed, 0 skipped, 0 dropped records). Standard heap alloc objects/eval dropped from 99.0 (June 28) to **29.5** — a **70% reduction**.

Pipelined mode shows **77.4% of Valkey** for IPC (up from 72.8% in the pre-drain-pool arena run). The drain buffer pool reduced heap alloc bytes/eval by **25.7%** (5,931 → 4,404). Remaining gap to June 28's 82.4% is from per-command telemetry model (10x more drain operations in pipelined mode vs per-batch).

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
| GoCache images | `gocache-bench:local` / `gocache-bench:local-ipc` |

### 2.2 Configurations Tested

| Configuration | Target | Label stem | Notes |
|---|---|---|---|
| Valkey | `valkey` | `drainpool-valkey` | Reference server |
| GoCache core | `gocache` | `drainpool-gocache` | No IPC plugins |
| GoCache IPC | `gocache-ipc` | `drainpool-gocache-ipc` | Prometheus IPC plugin |
| GoCache IPC pprof/stats | `gocache-ipc` | `drainpool-pprof-gocache-ipc` | `BENCH_PPROF=1`, `BENCH_STATS=1` |
| GoCache IPC+OTel | `gocache-ipc-otel` | `drainpool-otel-gocache-ipc-otel` | Prometheus + instrumentation + OTel Collector |

---

## 3. Results

### 3.1 Throughput & Latency Summary

Average across all 15 `valkey-benchmark --csv` rows.

| Configuration | Mode | Avg RPS | Avg Latency | Avg P99 | % of Valkey |
|---|---:|---:|---:|---:|---:|
| **Valkey** | Standard | **114,238** | **0.230ms** | **0.401ms** | **100.0%** |
| GoCache core | Standard | 112,336 | 0.236ms | 0.473ms | 98.3% |
| GoCache IPC | Standard | 103,846 | 0.250ms | 0.379ms | 90.9% |
| GoCache IPC pprof/stats | Standard | 106,395 | 0.245ms | 0.379ms | 93.1% |
| GoCache IPC+OTel | Standard | 105,728 | 0.246ms | 0.415ms | 92.5% |
| **Valkey** | Pipelined | **845,171** | **0.494ms** | **1.044ms** | **100.0%** |
| GoCache core | Pipelined | 681,645 | 0.611ms | 2.856ms | 80.7% |
| GoCache IPC | Pipelined | 653,714 | 0.716ms | 3.064ms | 77.4% |
| GoCache IPC pprof/stats | Pipelined | 642,330 | 0.744ms | 3.328ms | 76.0% |
| GoCache IPC+OTel | Pipelined | 647,642 | 0.741ms | 3.375ms | 76.6% |

### 3.2 Per-Command Detail

#### 3.2.1 RPS by command, configuration, and mode

| Test | Valkey Std | Valkey Pipe | Core Std | Core Pipe | IPC Std | IPC Pipe | pprof Std | pprof Pipe | OTel Std | OTel Pipe |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| PING_INLINE | 114,943 | 909,091 | 122,850 | 943,396 | 111,235 | 980,392 | 116,414 | 900,901 | 110,132 | 900,901 |
| PING_MBULK | 113,895 | 1,063,830 | 107,296 | 980,392 | 109,290 | 952,381 | 111,483 | 970,874 | 113,250 | 900,901 |
| SET | 116,822 | 943,396 | 116,279 | 900,901 | 112,233 | 934,579 | 105,820 | 877,193 | 105,152 | 869,565 |
| GET | 118,624 | 1,052,632 | 117,647 | 961,538 | 108,225 | 970,874 | 106,383 | 909,091 | 108,460 | 909,091 |
| INCR | 115,207 | 1,041,667 | 115,473 | 917,431 | 107,643 | 800,000 | 105,932 | 840,336 | 104,932 | 781,250 |
| LPUSH | 117,925 | 1,123,596 | 116,550 | 666,667 | 107,411 | 628,931 | 103,950 | 613,497 | 104,712 | 628,931 |
| RPUSH | 114,548 | 952,381 | 116,550 | 628,931 | 107,181 | 581,395 | 110,375 | 574,713 | 114,943 | 602,410 |
| LPOP | 115,607 | 775,194 | 114,155 | 628,931 | 109,290 | 621,118 | 113,507 | 621,118 | 102,249 | 649,351 |
| RPOP | 115,875 | 854,701 | 117,233 | 657,895 | 104,384 | 625,000 | 112,994 | 609,756 | 107,991 | 617,284 |
| SADD | 119,190 | 862,069 | 110,988 | 609,756 | 103,950 | 564,972 | 107,759 | 564,972 | 108,225 | 595,238 |
| HSET | 118,624 | 826,446 | 114,548 | 598,802 | 92,764 | 552,486 | 108,460 | 540,541 | 106,610 | 574,713 |
| SPOP | 113,507 | 617,284 | 110,497 | 657,895 | 95,969 | 591,716 | 105,263 | 598,802 | 106,383 | 645,161 |
| LPUSH (LRANGE) | 120,192 | 1,111,111 | 115,741 | 632,911 | 98,135 | 598,802 | 110,132 | 598,802 | 109,529 | 625,000 |
| LRANGE_100 | 81,235 | 214,133 | 78,003 | 192,308 | 78,003 | 172,117 | 74,683 | 185,529 | 74,294 | 190,114 |
| MSET (10 keys) | 117,371 | 330,033 | 111,235 | 246,914 | 111,982 | 230,947 | 102,775 | 228,833 | 109,051 | 224,719 |

#### 3.2.2 Latency (avg) by command, configuration, and mode

| Test | Valkey Std | Valkey Pipe | Core Std | Core Pipe | IPC Std | IPC Pipe | pprof Std | pprof Pipe | OTel Std | OTel Pipe |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| PING_INLINE | 0.226 | 0.337 | 0.212 | 0.409 | 0.233 | 0.431 | 0.223 | 0.460 | 0.237 | 0.499 |
| SET | 0.224 | 0.477 | 0.224 | 0.489 | 0.232 | 0.472 | 0.246 | 0.534 | 0.248 | 0.546 |
| GET | 0.218 | 0.406 | 0.222 | 0.449 | 0.241 | 0.440 | 0.247 | 0.481 | 0.241 | 0.512 |
| INCR | 0.227 | 0.416 | 0.225 | 0.468 | 0.243 | 0.600 | 0.247 | 0.555 | 0.249 | 0.623 |
| LPUSH | 0.219 | 0.325 | 0.224 | 0.737 | 0.242 | 0.782 | 0.250 | 0.804 | 0.248 | 0.785 |
| RPUSH | 0.227 | 0.353 | 0.224 | 0.781 | 0.244 | 0.845 | 0.237 | 0.861 | 0.229 | 0.818 |
| LRANGE_100 | 0.315 | 1.184 | 0.341 | 1.808 | 0.348 | 1.966 | 0.359 | 1.858 | 0.367 | 1.902 |
| MSET (10 keys) | 0.324 | 1.412 | 0.262 | 2.013 | 0.244 | 2.149 | 0.274 | 2.173 | 0.264 | 2.214 |

### 3.3 Memory Usage (RSS)

| Configuration | Baseline | Post-Standard | Final | Delta |
|---|---:|---:|---:|---:|
| Valkey | 9.2 MB | 25.2 MB | 27.5 MB | +18.2 MB |
| GoCache core | 40.4 MB | 182.9 MB | 271.5 MB | +231.1 MB |
| GoCache IPC | 45.8 MB | 186.4 MB | 268.9 MB | +223.1 MB |
| GoCache IPC pprof/stats | 43.8 MB | 182.7 MB | 280.7 MB | +236.9 MB |
| GoCache IPC+OTel | 55.1 MB | 200.4 MB | 291.0 MB | +235.9 MB |

#### OTel Collector Memory

| Metric | Value |
|---|---:|
| Baseline RSS | 40.5 MB |
| Final RSS | 40.5 MB |
| Delta | -0.01 MB (stable) |

### 3.4 Runtime Metrics (benchstats)

#### Standard Mode (pprof/stats variant)

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
| `pipeline.event.operation_completed` | 1,500,000 | **1.000** |
| `pipeline.event.operation_started` | 1,500,000 | **1.000** |
| `runtime.gc.heap.allocs.bytes` | 3,249,537,728 | 2,167 |
| `runtime.gc.heap.allocs.objects` | 44,318,359 | **29.5** |
| `runtime.gc.heap.objects.objects` | 1,233,760 | 0.8 |
| `runtime.memory.classes.heap.objects.bytes` | 123,677,488 | 82.5 |
| `runtime.memory.classes.total.bytes` | 181,909,800 | 121.3 |
| `runtime.sched.goroutines.goroutines` | 24 | — |
| `runtime.sync.mutex.wait.total.seconds` | 1.89s | 1.3 µs |

#### Pipelined Mode (pprof/stats variant)

| Metric | Value | Per-Evaluation |
|---|---:|---:|
| `enabled` | true | — |
| `manager.event_dropped` | 0 | 0 |
| `manager.event_enqueue_attempts` | 0 | 0 |
| `manager.event_received` | 0 | 0 |
| `manager.projection_builds` | 0 | 0 |
| `operation_tracker.dropped_completed` | 0 | 0 |
| `operation_tracker.dropped_records` | 0 | 0 |
| `operation_tracker.skipped_operations` | 24,577 | 0.016 |
| `pipeline.evaluations` | 1,500,000 | — |
| `pipeline.event.operation_completed` | 282,340 | 0.188 |
| `pipeline.event.operation_started` | 282,340 | 0.188 |
| `runtime.gc.heap.allocs.bytes` | 6,606,241,008 | 4,404 |
| `runtime.gc.heap.allocs.objects` | 81,326,745 | 54.2 |
| `runtime.gc.heap.objects.objects` | 1,395,763 | 0.9 |
| `runtime.memory.classes.heap.objects.bytes` | 149,370,288 | 99.6 |
| `runtime.memory.classes.total.bytes` | 285,407,560 | 190.3 |
| `runtime.sched.goroutines.goroutines` | 24 | — |
| `runtime.sync.mutex.wait.total.seconds` | 87.40s | 58.3 µs |

### 3.5 Pprof vs Clean IPC Comparison

| Metric | IPC Clean | IPC Pprof/Stats | Diff |
|---|---:|---:|---:|
| Standard RPS | 103,846 | 106,395 | +2.5% |
| Pipelined RPS | 653,714 | 642,330 | -1.7% |
| Standard latency | 0.250ms | 0.245ms | -2.0% |
| Pipelined latency | 0.716ms | 0.744ms | +3.9% |
| Final RSS | 268.9 MB | 280.7 MB | +4.4% |

---

## 4. Comparison with Previous Runs

### 4.1 Throughput Comparison (3 runs)

| Metric | Jun 28 (pre-arena) | Jun 30-02 (arena) | Jun 30-03 (drain pool) | Pool Impact |
|---|---:|---:|---:|---:|
| Valkey standard | 114,806 | 115,607 | 114,238 | — |
| Valkey pipelined | 924,080 | 899,425 | 845,171 | — |
| IPC standard %Valkey | 93.0% | 93.1% | 90.9% | -2.2pp |
| IPC pipelined %Valkey | 82.4% | 72.8% | **77.4%** | **+4.6pp** |
| OTel standard %Valkey | 89.0% | 92.0% | 92.5% | +0.5pp |
| OTel pipelined %Valkey | 78.9% | 67.0% | **76.6%** | **+9.6pp** |

### 4.2 Benchstats Comparison (pprof pipelined)

| Metric | Jun 28 | Jun 30-02 (arena) | Jun 30-03 (drain pool) |
|---|---:|---:|---:|
| Op events / eval | 0.186 | 0.191 | 0.188 |
| Skipped operations | 21,677 | 17,978 | 24,577 |
| Dropped records | 0 | 0 | **0** |
| Heap alloc objects / eval | 50.9 | 55.4 | 54.2 |
| Heap alloc bytes / eval | 3,920 | 5,931 | **4,404** |
| Mutex wait / eval | 46.8 µs | 59.4 µs | 58.3 µs |

### 4.3 Standard Mode Benchstats Comparison

| Metric | Jun 28 Standard | Jun 30-03 Standard | Delta |
|---|---:|---:|---:|
| Op events / eval | 1.000 | **1.000** | — |
| Skipped operations | 0 | **0** | — |
| Heap alloc objects / eval | 99.0 | **29.5** | **-70.2%** |
| Heap alloc bytes / eval | 5,694 | **2,167** | **-61.9%** |
| Mutex wait / eval | 4.0 µs | **1.3 µs** | **-67.5%** |

---

## 5. Key Findings

1. **Standard mode is excellent** — 100% telemetry coverage, 0 skips, 0 drops. Heap alloc objects/eval dropped 70% vs June 28 (99.0 → 29.5). Mutex wait dropped 67% (4.0μs → 1.3μs). Arena + drain pool eliminated pre-allocation waste and most drain-path allocations.
2. **Pipelined mode recovered partially** — drain buffer pool improved OTel pipelined from 67.0% to 76.6% of Valkey (+9.6pp). IPC improved from 72.8% to 77.4% (+4.6pp). Still ~5pp below June 28's 82.4%.
3. **Drain buffer pool cut allocations 25.7%** — heap alloc bytes/eval dropped from 5,931 (arena without pool) to 4,404 (with pool). Buffers recycled after warmup.
4. **Zero dropped records across ALL runs** — arena pool never exhausted. Dynamic growth works correctly.
5. **Remaining gap is structural** — per-command telemetry model creates 10x more drain operations in pipelined mode (pipeline depth 10). The sequential drain worker can't recycle slots fast enough, causing 81% skip rate in pipelined mode.
6. **OTel overhead is small** — OTel pipelined (76.6%) is within 1pp of clean IPC (77.4%). The instrumentation plugin adds negligible overhead.
7. **Memory is comparable** — IPC final RSS 268.9 MB vs June 28's 239.7 MB (+12%). The arena eliminates pre-allocation waste but adds per-operation arena structs.

---

## 6. Root Cause: Pipelined Telemetry Coverage

Standard mode achieves 100% coverage. Pipelined mode only 18.8%. The root cause is **drain worker throughput vs command throughput**:

- 50 clients × pipeline depth 10 = ~500 concurrent operations
- 8 shards × 256 slots = 2048 total slots (plenty for 500 ops)
- BUT: drain worker processes completed operations sequentially
- At 650K+ pipelined RPS, ~650K operations complete per second
- Single drain worker can process ~120K operations/sec (282K/1.5M in ~12s)
- Gap: 650K - 120K = 530K operations/sec can't get slots → skipped

**Options to close the gap:**
1. Parallel drain workers (multiple workers per shard)
2. Batch drain (process multiple operations per tmpfs write)
3. Hybrid telemetry model (per-command standard, per-batch pipelined)

---

## 7. Raw Data Files

```text
bench/task-results/2026-06-30-03-perf-drain-buffer-pool/
├── BENCHMARK_SUMMARY.md
├── drainpool-valkey-valkey.csv / -pipelined.csv / -memory.txt
├── drainpool-gocache.csv / -pipelined.csv / -memory.txt
├── drainpool-gocache-ipc.csv / -pipelined.csv / -memory.txt / -config.yaml
├── drainpool-gocache-ipc-telemetry-baseline/standard/pipelined.json
├── drainpool-pprof-gocache-ipc.csv / -pipelined.csv / -memory.txt / -config.yaml
├── drainpool-pprof-gocache-ipc-benchstats-baseline/standard/pipelined.json
├── drainpool-pprof-gocache-ipc-telemetry-baseline/standard/pipelined.json
├── drainpool-otel-gocache-ipc-otel.csv / -pipelined.csv / -memory.txt
├── drainpool-otel-gocache-ipc-otel-config.yaml / -otel-collector.yaml
└── drainpool-otel-gocache-ipc-otel-telemetry-baseline/standard/pipelined.json
```

---

*Generated from benchmark runs on 2026-06-30. Branch: perf/telemetry-processing (Phase 6 — Record Arena + Drain Buffer Pool).*
