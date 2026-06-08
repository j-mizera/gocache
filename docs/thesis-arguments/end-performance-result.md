# GoCache Telemetry Performance: Thesis Arguments

## Executive Summary

GoCache is a Redis-compatible in-memory cache server with a microkernel architecture. The telemetry system was redesigned to achieve zero-allocation operation submission, event-driven drain, and always-on observability. The system is always emitted regardless of plugin configuration, with a 92ns submit path and 0 allocations per operation.

The performance comparison against Valkey (a Redis fork optimized for performance) reveals that GoCache is competitive on single-command workloads but falls behind on pipelined operations, with the gap widening significantly when the OTEL instrumentation plugin is active.

---

## Performance Comparison: GoCache vs Valkey

### Methodology

All benchmarks ran with `BENCH_REPEAT=3` for statistical reliability. Tests compared three GoCache configurations against Valkey:

1. **GoCache core**: Plain server, no telemetry plugins
2. **GoCache + Prometheus**: IPC plugin with Prometheus metrics
3. **GoCache + OTEL instrumentation**: IPC plugin with OpenTelemetry instrumentation
4. **Valkey**: Redis-compatible fork as baseline

---

### Non-Pipelined Results (median RPS, p50 latency in ms)

| Command | GoCache core RPS | GoCache core p50 lat | GoCache+Prom RPS | GoCache+Prom p50 lat | GoCache+OTEL RPS | GoCache+OTEL p50 lat | Valkey RPS | Valkey p50 lat |
|:---|:---|:---|:---|:---|:---|:---|:---|:---|
| PING_INLINE | 111,857 (-5.3%) | 0.223 (+3.7%) | 113,895 (-3.5%) | 0.231 (+7.4%) | 95,329 (-19.3%) | 0.255 (+18.6%) | 118,064 | 0.215 |
| PING_MBULK | 113,636 (-2.7%) | 0.223 (+3.7%) | 108,108 (-7.5%) | 0.231 (+7.4%) | 95,877 (-17.9%) | 0.255 (+18.6%) | 116,822 | 0.215 |
| SET | 110,011 (-7.9%) | 0.231 (+7.4%) | 108,460 (-9.2%) | 0.231 (+7.4%) | 98,619 (-17.5%) | 0.255 (+18.6%) | 119,474 | 0.215 |
| GET | 111,235 (-4.3%) | 0.231 (+7.4%) | 100,200 (-13.8%) | 0.255 (+18.6%) | 96,805 (-16.7%) | 0.247 (+14.9%) | 116,279 | 0.215 |
| INCR | 110,132 (-6.3%) | 0.231 (+7.4%) | 112,108 (-4.6%) | 0.231 (+7.4%) | 96,525 (-17.9%) | 0.255 (+18.6%) | 117,509 | 0.215 |
| LPUSH | 111,732 (-1.2%) | 0.231 (+3.6%) | 110,254 (-2.5%) | 0.231 (+3.6%) | 95,785 (-15.3%) | 0.255 (+14.3%) | 113,122 | 0.223 |
| RPUSH | 111,607 (-6.3%) | 0.231 (+7.4%) | 111,857 (-6.0%) | 0.231 (+7.4%) | 96,712 (-18.8%) | 0.255 (+18.6%) | 119,048 | 0.215 |
| LPOP | 112,867 (+5.2%) | 0.231 (+0.0%) | 110,742 (+3.2%) | 0.231 (+0.0%) | 98,328 (-8.4%) | 0.255 (+10.4%) | 107,296 | 0.231 |
| RPOP | 109,890 (-6.3%) | 0.231 (+3.6%) | 110,497 (-5.7%) | 0.231 (+3.6%) | 96,154 (-18.0%) | 0.255 (+14.3%) | 117,233 | 0.223 |
| SADD | 109,409 (-3.9%) | 0.231 (+3.6%) | 113,250 (-0.6%) | 0.231 (+3.6%) | 97,087 (-14.8%) | 0.255 (+14.3%) | 113,895 | 0.223 |
| HSET | 114,025 (-2.9%) | 0.231 (+3.6%) | 113,379 (-3.4%) | 0.231 (+3.6%) | 95,420 (-18.7%) | 0.255 (+14.3%) | 117,371 | 0.223 |
| SPOP | 108,696 (-7.2%) | 0.231 (+7.4%) | 109,170 (-6.8%) | 0.231 (+7.4%) | 97,182 (-17.0%) | 0.255 (+18.6%) | 117,096 | 0.215 |
| LPUSH (LRANGE setup) | 112,486 (-0.3%) | 0.231 (+3.6%) | 106,724 (-5.4%) | 0.231 (+3.6%) | 97,752 (-13.4%) | 0.255 (+14.3%) | 112,867 | 0.223 |
| LRANGE_100 | 73,046 (-9.4%) | 0.335 (+7.7%) | 76,161 (-5.6%) | 0.327 (+5.1%) | 65,963 (-18.2%) | 0.359 (+15.4%) | 80,645 | 0.311 |
| MSET (10 keys) | 106,610 (-6.7%) | 0.239 (+0.0%) | 108,342 (-5.2%) | 0.239 (+0.0%) | 84,602 (-26.0%) | 0.311 (+30.1%) | 114,286 | 0.239 |

---

### Pipelined Results (median RPS, p50 latency in ms)

| Command | GoCache core RPS | GoCache core p50 lat | GoCache+Prom RPS | GoCache+Prom p50 lat | GoCache+OTEL RPS | GoCache+OTEL p50 lat | Valkey RPS | Valkey p50 lat |
|:---|:---|:---|:---|:---|:---|:---|:---|:---|
| PING_INLINE | 980,392 (+9.8%) | 0.295 (-5.1%) | 900,901 (+0.9%) | 0.319 (+2.6%) | 598,802 (-32.9%) | 0.415 (+33.4%) | 892,857 | 0.311 |
| PING_MBULK | 909,091 (-9.1%) | 0.319 (+29.1%) | 884,956 (-11.5%) | 0.327 (+32.4%) | 628,931 (-37.1%) | 0.407 (+64.8%) | 1,000,000 | 0.247 |
| SET | 689,655 (-11.0%) | 0.303 (-37.8%) | 636,943 (-17.8%) | 0.319 (-34.5%) | 347,222 (-55.2%) | 0.911 (+87.1%) | 775,194 | 0.487 |
| GET | 854,701 (+6.0%) | 0.399 (-24.3%) | 833,333 (+3.3%) | 0.447 (-15.2%) | 523,560 (-35.1%) | 0.487 (-7.6%) | 806,452 | 0.527 |
| INCR | 694,444 (-11.8%) | 0.311 (-43.6%) | 621,118 (-21.1%) | 0.343 (-37.7%) | 326,797 (-58.5%) | 0.927 (+68.2%) | 787,402 | 0.551 |
| LPUSH | 487,805 (-52.2%) | 1.119 (+218.8%) | 431,034 (-57.8%) | 1.159 (+230.2%) | 304,878 (-70.1%) | 1.311 (+273.5%) | 1,020,408 | 0.351 |
| RPUSH | 434,783 (-53.5%) | 1.151 (+270.1%) | 408,163 (-56.3%) | 1.191 (+283.0%) | 272,480 (-70.8%) | 1.431 (+360.1%) | 934,579 | 0.311 |
| LPOP | 492,611 (-47.8%) | 1.119 (+154.9%) | 442,478 (-53.1%) | 1.143 (+160.4%) | 294,118 (-68.8%) | 1.383 (+215.0%) | 943,396 | 0.439 |
| RPOP | 485,437 (-47.6%) | 1.119 (+186.2%) | 434,783 (-53.0%) | 1.159 (+196.4%) | 302,115 (-67.4%) | 1.407 (+259.8%) | 925,926 | 0.391 |
| SADD | 458,716 (-42.2%) | 1.135 (+149.5%) | 411,523 (-48.1%) | 1.183 (+160.0%) | 284,900 (-64.1%) | 1.463 (+221.5%) | 793,651 | 0.455 |
| HSET | 425,532 (-47.2%) | 1.151 (+112.0%) | 403,226 (-50.0%) | 1.183 (+117.9%) | 258,398 (-68.0%) | 1.519 (+179.7%) | 806,452 | 0.543 |
| SPOP | 465,116 (-15.8%) | 1.119 (+50.6%) | 423,729 (-23.3%) | 1.167 (+57.1%) | 286,533 (-48.1%) | 1.343 (+80.8%) | 552,486 | 0.743 |
| LPUSH (LRANGE setup) | 465,116 (-36.7%) | 1.127 (+90.7%) | 425,532 (-42.1%) | 1.159 (+96.1%) | 284,091 (-61.4%) | 1.335 (+125.9%) | 735,294 | 0.591 |
| LRANGE_100 | 181,818 (+6.4%) | 1.255 (-0.6%) | 175,131 (+2.5%) | 1.279 (+1.3%) | 108,578 (-36.5%) | 2.639 (+108.9%) | 170,940 | 1.263 |
| MSET (10 keys) | 239,234 (-5.0%) | 1.719 (-7.3%) | 225,734 (-10.4%) | 1.751 (-5.6%) | 105,820 (-58.0%) | 3.983 (+114.7%) | 251,889 | 1.855 |

---

## Key Findings

### 1. Telemetry Is Always Emitted

The telemetry system operates unconditionally:
- **92ns submit path**, **0 allocs/op**
- No interest-gating: the slot is always acquired and the operation record is always written
- Even with no external sink (no Prometheus, no OTEL), the infrastructure is fully active
- Decision: keep always-on observability; the overhead is acceptable compared to conditional complexity

### 2. Competitive on Non-Pipelined Workloads

GoCache core is within **~5-10% of Valkey** for most single-command operations:
- p50 latency is only **~3-7% higher**
- Prometheus adds minimal overhead (~0-10%)
- OTEL adds significant overhead (~15-20%)

### 3. Large Gap on Pipelined Workloads

The gap widens dramatically for pipelined operations:
- **List operations (LPUSH, RPUSH, LPOP, RPOP)**: GoCache is **45-70% slower** than Valkey
- **p50 latency for list ops**: 2-3x higher than Valkey
- The bottleneck is the **core cache shard mutex**, not telemetry

### 4. OTEL Plugin Is the Dominant Bottleneck

When OTEL is active, the performance degradation is severe:
- **Pipelined list ops**: 58-71% slower than Valkey
- **OTEL p50 latency**: 68-360% higher than Valkey
- The IPC projection and materialization path cannot keep up with the drain cycle

---

## P6 Acceptance Matrix Results

| Gate | Description | Verdict |
|------|-------------|---------|
| G1 | Prom mutex wait <= 5s | **FAIL** (116.5s) |
| G2 | OTEL mutex wait <= 20s | **FAIL** (127.6s) |
| G3 | Prom allocs <= 55M | **FAIL** (84.8M) |
| G4 | OTEL allocs <= 120M | **FAIL** (305.7M) |
| G5 | Started events >= 99% | **FAIL** (Prom 98.8%, OTEL 59.8%) |
| G6 | Projection builds == accepted events | **PASS** |
| G7 | Prom RPS vs core >= -3% | **FAIL** (-7.5%) |
| G8 | OTEL RPS vs Prom >= -25% | **FAIL** (-36.6%) |
| G9 | OTEL p99 vs Prom <= +50% | **FAIL** (+435%) |
| G10 | Submit path 0 allocs/op | **PASS** |
| G11 | Race suite clean | **PASS** |
| G12 | Core vs Valkey <= -5% | **FAIL** (-27.3%) |

**Pass rate: 3/12 (G6, G10, G11)**

---

## F1-F4 Final Verification

### F2: Code Quality Review — APPROVED
- No race conditions
- Proper lock ordering
- No package boundary violations
- `go vet` passes, `go test -race` passes

### F3: Real Manual QA — APPROVED
- All benchmarks reproducible with `BENCH_REPEAT=3`
- Artifacts complete and documented

### F1: Plan Compliance Audit — REJECTED
- P2/P3 implementation contradiction
- P6 incomplete (missing gocache-core-probe run)
- P4 unverified

### F4: Scope Fidelity Check — REJECTED
- `replay.gap` schema changed without ADR
- Loss taxonomy incomplete (4 of 7 classes missing)
- IPC drop visibility not mapped to `replay.gap`

---

## Conclusion

The telemetry remediation successfully achieved its primary technical goal: **zero-allocation, sub-100ns operation submission**. The telemetry path itself is no longer the bottleneck.

However, the system still faces significant performance challenges:
1. **Core cache shard mutex** dominates on pipelined workloads, especially list operations
2. **OTEL plugin's IPC overhead** is devastating for high-throughput scenarios
3. The **~27% gap to Valkey** on pipelined throughput is a core engine issue, not telemetry

The thesis demonstrates that **safe extensibility and high performance can coexist** for the telemetry submit path, but the broader claim depends on resolving the core engine bottlenecks.

---

## Evidence References

- `.omo/evidence/telemetry-remediation/task-21-p6-acceptance-matrix.md` — Full P6 matrix
- `.omo/evidence/telemetry-remediation/task-21-failure-attribution.md` — Failure root cause analysis
- `.omo/evidence/telemetry-remediation/task-20-nosink-decision-packet.md` — P5 always-on decision
- `.omo/evidence/telemetry-remediation/task-22-consolidation.md` — Handoff document
- `bench/results/telemetry-remediation-p21-*` — Raw benchmark artifacts

---

*Commit: `37240842e4b8bac063c4f826a9dcb240605f8a24` | Branch: `zero-allocation-telemetry-subplan-b`*
<!-- OMO_INTERNAL_INITIATOR -->