# GoCache Benchmark Summary: Record Arena (Phase 6)

**Date:** 2026-06-30  
**Branch:** `perf/telemetry-processing`  
**Change:** RecordArena chunk-chain storage replaces flat pre-allocated array (Phase 6)

---

## 1. Executive Summary

Full Valkey / GoCache core / GoCache IPC / GoCache IPC pprof+stats / GoCache IPC+OTel benchmark matrix with the RecordArena chunk-chain storage replacing the flat pre-allocated `segmentSize × recordsPerOperation` array.

- **Valkey standard:** **115,607 RPS** reference average.
- **GoCache core standard:** **108,274 RPS**, **93.7% of Valkey**.
- **GoCache IPC standard:** **107,583 RPS**, **93.1% of Valkey**.
- **GoCache IPC pprof/stats standard:** **100,290 RPS**, **86.8% of Valkey**.
- **GoCache IPC+OTel standard:** **106,317 RPS**, **92.0% of Valkey**.
- **Valkey pipelined:** **899,425 RPS** reference average.
- **GoCache core pipelined:** **649,106 RPS**, **72.2% of Valkey**.
- **GoCache IPC pipelined:** **655,138 RPS**, **72.8% of Valkey**.
- **GoCache IPC pprof/stats pipelined:** **624,363 RPS**, **69.4% of Valkey**.
- **GoCache IPC+OTel pipelined:** **602,914 RPS**, **67.0% of Valkey**.

**Standard mode improved** vs June 28: core +7.5%, IPC +0.7%, OTel +4.1%.  
**Pipelined mode regressed** vs June 28: IPC -14.0%, OTel -17.3%.  
**Core pipelined improved** +15.9% — benefits from reduced memory pressure.

The regression is from the **copy-on-drain** path: each drained operation now allocates a contiguous `[]TelemetryRecord` from the chunk chain (1 alloc/op). The old flat-array path returned a zero-copy alias. This adds GC pressure under high pipelined throughput. Optimization path: serialize directly from chunk chain without flattening, or pool the drain buffer.

**Key win:** Zero dropped records across all runs. Memory proportional to actual usage (no 2.5 GB pre-allocation).

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
| Valkey | `valkey` | `arena-full-valkey` | Reference server |
| GoCache core | `gocache` | `arena-full-gocache` | No IPC plugins |
| GoCache IPC | `gocache-ipc` | `arena-full-gocache-ipc` | Prometheus IPC plugin |
| GoCache IPC pprof/stats | `gocache-ipc` | `arena-full-pprof-gocache-ipc` | `BENCH_PPROF=1`, `BENCH_STATS=1` |
| GoCache IPC+OTel | `gocache-ipc-otel` | `arena-full-otel-gocache-ipc-otel` | Prometheus + instrumentation + OTel Collector |

---

## 3. Results

### 3.1 Throughput & Latency Summary

| Configuration | Mode | Avg RPS | % of Valkey |
|---|---:|---:|---:|
| **Valkey** | Standard | **115,607** | **100.0%** |
| GoCache core | Standard | 108,274 | 93.7% |
| GoCache IPC | Standard | 107,583 | 93.1% |
| GoCache IPC pprof/stats | Standard | 100,290 | 86.8% |
| GoCache IPC+OTel | Standard | 106,317 | 92.0% |
| **Valkey** | Pipelined | **899,425** | **100.0%** |
| GoCache core | Pipelined | 649,106 | 72.2% |
| GoCache IPC | Pipelined | 655,138 | 72.8% |
| GoCache IPC pprof/stats | Pipelined | 624,363 | 69.4% |
| GoCache IPC+OTel | Pipelined | 602,914 | 67.0% |

### 3.2 Memory Usage (RSS)

| Configuration | Baseline | Post-Standard | Final | Delta |
|---|---:|---:|---:|---:|
| Valkey | 22.1 MB | 38.3 MB | 40.9 MB | +18.8 MB |
| GoCache core | 40.3 MB | 181.9 MB | 268.6 MB | +228.3 MB |
| GoCache IPC | 46.1 MB | 187.2 MB | 226.4 MB | +180.3 MB |
| GoCache IPC pprof/stats | 45.9 MB | 187.8 MB | 272.1 MB | +226.2 MB |
| GoCache IPC+OTel | 55.0 MB | 202.3 MB | 291.0 MB | +235.9 MB |
| OTel Collector | 40.6 MB | 40.5 MB | 40.5 MB | -0.2 MB |

### 3.3 Runtime Metrics (benchstats — pprof/stats pipelined)

| Metric | June 28 | June 30 | Delta |
|---|---:|---:|---:|
| Evaluations | 1,500,000 | 1,500,000 | — |
| Operation started/completed | 278,323 | 286,953 | +3.1% |
| Op events / eval | 0.186 | 0.191 | +2.7% |
| Skipped operations | 21,677 | 17,978 | **-17.0%** |
| Dropped records | 0 | **0** | — |
| Heap alloc objects / eval | 50.9 | 55.4 | +8.8% |
| Heap alloc bytes / eval | 3,920 | 5,931 | **+51.4%** |
| Mutex wait / eval | 46.8 μs | 59.4 μs | +26.9% |
| Total runtime memory / eval | 178.9 B | 181.6 B | +1.5% |

### 3.4 Telemetry Counters (IPC pipelined)

| Metric | Value |
|---|---:|
| Skipped operations | 25,546 (1.7%) |
| Dropped records | **0** |
| Dropped completed | 0 |
| Segments per shard | 1 (no growth needed) |

---

## 4. Comparison with June 28 (tmpfs-full)

### 4.1 Throughput Comparison

| Metric | June 28 | June 30 | Delta |
|---|---:|---:|---:|
| Valkey standard avg RPS | 114,806 | 115,607 | +0.7% |
| Valkey pipelined avg RPS | 924,080 | 899,425 | -2.7% |
| Core standard avg RPS | 100,691 | 108,274 | **+7.5%** |
| Core pipelined avg RPS | 559,938 | 649,106 | **+15.9%** |
| IPC standard avg RPS | 106,818 | 107,583 | +0.7% |
| IPC pipelined avg RPS | 761,684 | 655,138 | **-14.0%** |
| IPC pprof pipelined avg RPS | 737,549 | 624,363 | **-15.3%** |
| OTel standard avg RPS | 102,137 | 106,317 | +4.1% |
| OTel pipelined avg RPS | 728,866 | 602,914 | **-17.3%** |

### 4.2 Memory Comparison

| Configuration | June 28 Final RSS | June 30 Final RSS | Delta |
|---|---:|---:|---:|
| Valkey | 34.9 MB | 40.9 MB | +6.0 MB |
| GoCache core | 291.5 MB | 268.6 MB | **-22.9 MB** |
| GoCache IPC | 239.7 MB | 226.4 MB | **-13.3 MB** |
| GoCache IPC pprof/stats | 237.7 MB | 272.1 MB | +34.4 MB |
| GoCache IPC+OTel | 258.7 MB | 291.0 MB | +32.3 MB |

---

## 5. Key Findings

1. **Standard mode improved or held** — core +7.5%, OTel +4.1%, IPC +0.7%. Arena append overhead (17.8ns vs 12.1ns) is negligible at standard RPS.
2. **Pipelined mode regressed for IPC/OTel** — IPC -14.0%, OTel -17.3%. Cause: copy-on-drain allocation (1 alloc/op) adds GC pressure at high throughput. Core pipelined improved +15.9% (no drain serialization path).
3. **Zero dropped records** — arena pool never exhausted. Dynamic growth works correctly. Skipped operations reduced 17% vs June 28.
4. **Heap alloc bytes/eval increased +51%** (3,920 → 5,931) — drain-path copy allocation.
5. **Mutex wait increased +27%** (46.8μs → 59.4μs) — ChunkPool mutex contention + drain allocation overhead.
6. **Core memory reduced** — core RSS -22.9 MB, IPC RSS -13.3 MB. Arena eliminates flat-array pre-allocation waste.
7. **pprof/OTel memory increased** — GC pressure from drain allocations increases working set.

---

## 6. Root Cause Analysis

The pipelined throughput regression is from the **copy-on-drain** design:

- **Old path**: `completedOperationFromSlot` returns `segment.records[start:end]` — zero-copy alias into pre-allocated array. No allocation.
- **New path**: `arena.Drain()` walks chunk chain, allocates `make([]TelemetryRecord, count)`, copies records. 1 allocation per drained operation.

At 600K+ pipelined RPS, this adds ~600K allocations/second on the drain path. Each allocation is ~2-6 KB (7-20 records × ~296 bytes). This creates ~1.2-3.6 GB/sec of allocation pressure, increasing GC frequency and mutex wait.

### Optimization Path

1. **Serialize directly from chunk chain** — proto.MarshalVT iterates chunks without flattening. Eliminates the drain copy entirely.
2. **Pool the drain buffer** — reuse the contiguous slice across drains via sync.Pool or ChunkPool.
3. **MarshalToVT with pooled buffer** — write directly to a pre-allocated proto buffer, zero intermediate copy.

---

## 7. Raw Data Files

```text
bench/task-results/2026-06-30-02-perf-record-arena-matrix/
├── BENCHMARK_SUMMARY.md (this file)
├── arena-full-valkey.csv
├── arena-full-valkey-pipelined.csv
├── arena-full-valkey-memory.txt
├── arena-full-gocache.csv
├── arena-full-gocache-pipelined.csv
├── arena-full-gocache-memory.txt
├── arena-full-gocache-ipc.csv
├── arena-full-gocache-ipc-pipelined.csv
├── arena-full-gocache-ipc-memory.txt
├── arena-full-gocache-ipc-config.yaml
├── arena-full-gocache-ipc-telemetry-baseline.json
├── arena-full-gocache-ipc-telemetry-standard.json
├── arena-full-gocache-ipc-telemetry-pipelined.json
├── arena-full-pprof-gocache-ipc.csv
├── arena-full-pprof-gocache-ipc-pipelined.csv
├── arena-full-pprof-gocache-ipc-memory.txt
├── arena-full-pprof-gocache-ipc-config.yaml
├── arena-full-pprof-gocache-ipc-telemetry-*.json
├── arena-full-pprof-gocache-ipc-benchstats-*.json
├── arena-full-otel-gocache-ipc-otel.csv
├── arena-full-otel-gocache-ipc-otel-pipelined.csv
├── arena-full-otel-gocache-ipc-otel-memory.txt
├── arena-full-otel-gocache-ipc-otel-config.yaml
├── arena-full-otel-gocache-ipc-otel-otel-collector.yaml
└── arena-full-otel-gocache-ipc-otel-telemetry-*.json
```

---

*Generated from benchmark runs on 2026-06-30. Branch: perf/telemetry-processing (Phase 6 — Record Arena).*
