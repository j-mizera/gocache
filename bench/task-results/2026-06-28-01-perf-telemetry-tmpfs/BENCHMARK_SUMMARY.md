# GoCache Benchmark Summary: Telemetry tmpfs Shared-Memory IPC (ADR-0037)

**Date:** 2026-06-28  
**Branch:** `perf/telemetry-tmpfs`  
**Label:** `tmpfs-otel`  
**Iterations:** 3 (median reported)

---

## 1. Executive Summary

The tmpfs telemetry IPC architecture (ADR-0037) delivers dramatic pipelined throughput improvement by eliminating the drain worker's context materialization bottleneck.

- **GoCache IPC+OTel standard mode:** **~113K avg RPS** (consistent with June 9 baseline)
- **GoCache IPC+OTel pipelined mode:** **~793K avg RPS** — **+64% vs June 9** (484K avg)

The pipelined improvement ranges from +12% (PING) to +112% (SET). The ~53% pipelined skip rate from T-PARALLEL is eliminated — the drain worker now serializes protobuf bytes instead of building context maps.

## 2. Comparison with June 9 Baseline (IPC+OTel)

### Standard Mode

| Test | June 9 | June 28 (median) | Delta |
|------|--------|-------------------|-------|
| PING_INLINE | 114,155 | 121,507 | +6.4% |
| SET | 113,507 | 117,925 | +3.9% |
| GET | 112,108 | 111,607 | -0.4% |
| INCR | 98,135 | 114,025 | +16.2% |
| LPUSH | 108,578 | 109,890 | +1.2% |
| LRANGE_100 | 71,891 | 77,340 | +7.6% |
| MSET (10 keys) | 104,603 | 104,275 | -0.3% |

### Pipelined Mode (P=10)

| Test | June 9 | June 28 (median) | Delta |
|------|--------|-------------------|-------|
| PING_INLINE | 769,231 | 862,069 | +12.1% |
| SET | 448,430 | 952,381 | +112.4% |
| GET | 675,676 | 1,020,408 | +51.0% |
| INCR | 490,196 | 943,396 | +92.5% |
| LPUSH | 420,168 | 806,452 | +91.9% |
| RPUSH | 436,681 | 704,225 | +61.3% |
| LPOP | 518,135 | 793,651 | +53.2% |
| RPOP | 473,934 | 813,008 | +71.6% |
| SADD | 465,116 | 735,294 | +58.1% |
| HSET | 438,597 | 704,225 | +60.5% |
| SPOP | 492,611 | 724,638 | +47.1% |
| LRANGE_100 | 152,439 | 189,394 | +24.2% |
| MSET (10 keys) | 173,611 | 212,314 | +22.3% |

## 3. Methodology

| Parameter | Value |
|---|---:|
| Operations per test | 100,000 |
| Clients | 50 |
| Keyspace | 100,000 |
| Pipeline depth | 10 |
| Target CPUs | 0-3 |
| Client CPUs | 4-7 |
| Container memory | 2g |
| Iterations | 3 (median reported) |
| IPC plugins | prometheus + instrumentation |
| OTel Collector | nop pipeline |

## 4. Memory

| Metric | Value |
|---|---:|
| Target baseline RSS | ~68 MB |
| Target final RSS | ~247 MB |
| Target delta | ~+179 MB |
| OTel Collector RSS | ~42 MB (stable) |

## 5. Raw Data Files

- `tmpfs-otel-gocache-ipc-otel.csv` (standard, median run)
- `tmpfs-otel-gocache-ipc-otel-pipelined.csv` (pipelined, median run)
- `tmpfs-otel-gocache-ipc-otel-memory.txt`
- `tmpfs-otel-gocache-ipc-otel-config.yaml`
- `tmpfs-otel-gocache-ipc-otel-otel-collector.yaml`
- `repeats/standard/repeat-{1,2,3}.csv`
- `repeats/pipelined/repeat-{1,2,3}.csv`

---

*Generated from benchmark runs on 2026-06-28. Branch: perf/telemetry-tmpfs (ADR-0037).*
