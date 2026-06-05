# OperationTracker telemetry post-change benchmark summary

Date: 2026-06-05  
Commit: dc43956ca12b8f4cc609be51eef2a4c8aac64b54  
Branch: zero-allocation-telemetry-subplan-b  
Harness: `bench/redis-benchmark/run.sh` and `bench/redis-benchmark/run-ipc.sh` with Dockerized `valkey-benchmark`.

## Commands run

```bash
RESULTS_DIR="$PWD/bench/results/zero-allocation-telemetry-core-20260605" REBUILD=1 ./bench/redis-benchmark/run.sh operationtracker-telemetry-post --target gocache
RESULTS_DIR="$PWD/bench/results/zero-allocation-telemetry-core-20260605" REBUILD=0 ./bench/redis-benchmark/run.sh operationtracker-telemetry-post --target valkey
RESULTS_DIR="$PWD/bench/results/operationtracker-telemetry-post-otel" BENCH_STATS=1 REBUILD=1 GOCACHE_IPC_IMAGE=gocache-bench:operationtracker-telemetry-post-prom IPC_PLUGINS=prometheus GIT_MASTER=1 ./bench/redis-benchmark/run-ipc.sh operationtracker-telemetry-post --target gocache-ipc
RESULTS_DIR="$PWD/bench/results/operationtracker-telemetry-post-otel" BENCH_STATS=1 REBUILD=1 GOCACHE_IPC_IMAGE=gocache-bench:operationtracker-telemetry-post-otel GIT_MASTER=1 ./bench/redis-benchmark/run-ipc.sh operationtracker-telemetry-post --target gocache-ipc-otel
```

The GoCache runs emitted `WARNING: Could not fetch server CONFIG`; the harness continued and wrote CSV, memory, config, and benchstats outputs where applicable.

## Directional historical comparison

These compare against earlier result directories, not same-process repeated runs. Treat them as directional evidence, not statistical proof.

- core_std_hist: avg=-20.6% median=-22.4% range=-26.7%..-12.3%
- core_pipe_hist: avg=-39.2% median=-39.0% range=-48.3%..-29.2%
- valkey_std_hist: avg=-19.8% median=-21.1% range=-24.3%..-7.0%
- valkey_pipe_hist: avg=-25.6% median=-25.6% range=-42.4%..-18.4%
- otel_std_hist: avg=-4.7% median=-5.4% range=-11.4%..+3.4%
- otel_pipe_hist: avg=+27.3% median=+20.3% range=-4.9%..+105.5%
- prom_vs_core_std: avg=+0.3% median=+0.7% range=-4.0%..+3.6%
- prom_vs_core_pipe: avg=-5.1% median=-4.0% range=-13.8%..+0.6%
- otel_vs_prom_std: avg=-13.4% median=-12.6% range=-20.2%..-8.1%
- otel_vs_prom_pipe: avg=-48.8% median=-51.4% range=-59.1%..-27.4%
- otel_vs_core_pipe: avg=-51.3% median=-54.0% range=-63.8%..-28.7%

### Core vs historical core baseline — standard

| Test | baseline rps | post rps | Delta |
|---|---:|---:|---:|
| PING_INLINE | 134,048.27 | 98,231.83 | -26.7% |
| PING_MBULK | 134,228.19 | 100,100.10 | -25.4% |
| SET | 131,233.59 | 96,339.12 | -26.6% |
| GET | 128,534.70 | 98,231.83 | -23.6% |
| INCR | 126,262.62 | 98,039.22 | -22.4% |
| LPUSH | 117,233.30 | 99,800.40 | -14.9% |
| RPUSH | 129,198.97 | 97,276.27 | -24.7% |
| LPOP | 117,785.63 | 95,510.98 | -18.9% |
| RPOP | 115,606.94 | 100,704.94 | -12.9% |
| SADD | 111,607.14 | 97,560.98 | -12.6% |
| HSET | 112,739.57 | 98,911.96 | -12.3% |
| SPOP | 128,534.70 | 99,700.90 | -22.4% |
| LRANGE_100 (first 100 elements) | 85,397.09 | 65,019.51 | -23.9% |
| MSET (10 keys) | 121,951.22 | 96,061.48 | -21.2% |
| **Average** |  |  | **-20.6%** |
| **Median** |  |  | **-22.4%** |
| **Range** |  |  | **-26.7% to -12.3%** |

### Core vs historical core baseline — pipelined

| Test | baseline rps | post rps | Delta |
|---|---:|---:|---:|
| PING_INLINE | 1,219,512.12 | 781,249.94 | -35.9% |
| PING_MBULK | 1,234,567.88 | 847,457.62 | -31.4% |
| SET | 1,149,425.38 | 680,272.12 | -40.8% |
| GET | 1,176,470.62 | 833,333.38 | -29.2% |
| INCR | 1,162,790.62 | 729,927.06 | -37.2% |
| LPUSH | 909,090.94 | 500,000.00 | -45.0% |
| RPUSH | 781,249.94 | 442,477.88 | -43.4% |
| LPOP | 952,381.00 | 515,463.91 | -45.9% |
| RPOP | 943,396.25 | 515,463.91 | -45.4% |
| SADD | 833,333.38 | 431,034.50 | -48.3% |
| HSET | 775,193.81 | 436,681.22 | -43.7% |
| SPOP | 781,249.94 | 515,463.91 | -34.0% |
| LRANGE_100 (first 100 elements) | 234,741.78 | 155,521.00 | -33.7% |
| MSET (10 keys) | 298,507.47 | 192,678.23 | -35.5% |
| **Average** |  |  | **-39.2%** |
| **Median** |  |  | **-39.0%** |
| **Range** |  |  | **-48.3% to -29.2%** |

### Valkey current control vs historical Valkey baseline — standard

| Test | baseline rps | post rps | Delta |
|---|---:|---:|---:|
| PING_INLINE | 132,802.12 | 101,729.40 | -23.4% |
| PING_MBULK | 110,011.00 | 102,354.15 | -7.0% |
| SET | 129,032.27 | 101,936.80 | -21.0% |
| GET | 131,926.12 | 100,000.00 | -24.2% |
| INCR | 128,040.97 | 96,899.23 | -24.3% |
| LPUSH | 123,609.39 | 99,403.58 | -19.6% |
| RPUSH | 114,678.90 | 98,425.20 | -14.2% |
| LPOP | 128,205.13 | 98,619.32 | -23.1% |
| RPOP | 126,422.25 | 100,908.17 | -20.2% |
| SADD | 128,369.71 | 101,214.58 | -21.2% |
| HSET | 130,208.34 | 99,304.87 | -23.7% |
| SPOP | 132,626.00 | 104,058.27 | -21.5% |
| LRANGE_100 (first 100 elements) | 85,543.20 | 70,126.23 | -18.0% |
| MSET (10 keys) | 117,370.89 | 99,206.34 | -15.5% |
| **Average** |  |  | **-19.8%** |
| **Median** |  |  | **-21.1%** |
| **Range** |  |  | **-24.3% to -7.0%** |

### Valkey current control vs historical Valkey baseline — pipelined

| Test | baseline rps | post rps | Delta |
|---|---:|---:|---:|
| PING_INLINE | 1,265,822.75 | 925,925.88 | -26.9% |
| PING_MBULK | 1,282,051.25 | 952,381.00 | -25.7% |
| SET | 1,098,901.12 | 819,672.12 | -25.4% |
| GET | 1,234,567.88 | 854,700.88 | -30.8% |
| INCR | 1,149,425.38 | 819,672.12 | -28.7% |
| LPUSH | 1,265,822.75 | 1,010,101.00 | -20.2% |
| RPUSH | 1,250,000.00 | 1,020,408.19 | -18.4% |
| LPOP | 1,162,790.62 | 917,431.19 | -21.1% |
| RPOP | 1,190,476.25 | 934,579.44 | -21.5% |
| SADD | 1,136,363.62 | 833,333.38 | -26.7% |
| HSET | 1,111,111.12 | 800,000.00 | -28.0% |
| SPOP | 684,931.50 | 523,560.22 | -23.6% |
| LRANGE_100 (first 100 elements) | 236,406.61 | 191,204.59 | -19.1% |
| MSET (10 keys) | 411,522.62 | 236,966.83 | -42.4% |
| **Average** |  |  | **-25.6%** |
| **Median** |  |  | **-25.6%** |
| **Range** |  |  | **-42.4% to -18.4%** |

### Current Prometheus-only IPC vs current core — pipelined

| Test | core rps | prometheus-only ipc rps | Delta |
|---|---:|---:|---:|
| SET | 680,272.12 | 671,140.94 | -1.3% |
| GET | 833,333.38 | 769,230.81 | -7.7% |
| INCR | 729,927.06 | 628,930.81 | -13.8% |
| LPUSH | 500,000.00 | 458,715.59 | -8.3% |
| RPUSH | 442,477.88 | 431,034.50 | -2.6% |
| LPOP | 515,463.91 | 487,804.88 | -5.4% |
| RPOP | 515,463.91 | 505,050.50 | -2.0% |
| SADD | 431,034.50 | 431,034.50 | +0.0% |
| HSET | 436,681.22 | 413,223.16 | -5.4% |
| SPOP | 515,463.91 | 448,430.47 | -13.0% |
| LRANGE_100 (first 100 elements) | 155,521.00 | 152,671.77 | -1.8% |
| MSET (10 keys) | 192,678.23 | 193,798.45 | +0.6% |
| **Average** |  |  | **-5.1%** |
| **Median** |  |  | **-4.0%** |
| **Range** |  |  | **-13.8% to +0.6%** |

### Current Prometheus+instrumentation vs current Prometheus-only IPC — pipelined

| Test | prometheus-only ipc rps | prometheus+instrumentation rps | Delta |
|---|---:|---:|---:|
| SET | 671,140.94 | 274,725.28 | -59.1% |
| GET | 769,230.81 | 471,698.12 | -38.7% |
| INCR | 628,930.81 | 264,550.28 | -57.9% |
| LPUSH | 458,715.59 | 220,750.55 | -51.9% |
| RPUSH | 431,034.50 | 207,468.88 | -51.9% |
| LPOP | 487,804.88 | 227,272.73 | -53.4% |
| RPOP | 505,050.50 | 221,729.48 | -56.1% |
| SADD | 431,034.50 | 211,864.41 | -50.8% |
| HSET | 413,223.16 | 212,314.23 | -48.6% |
| SPOP | 448,430.47 | 232,558.14 | -48.1% |
| LRANGE_100 (first 100 elements) | 152,671.77 | 110,864.74 | -27.4% |
| MSET (10 keys) | 193,798.45 | 113,765.64 | -41.3% |
| **Average** |  |  | **-48.8%** |
| **Median** |  |  | **-51.4%** |
| **Range** |  |  | **-59.1% to -27.4%** |

## Pipelined benchstats notes

Prometheus-only `gocache-ipc` pipelined benchstats selected counters:

```json
{
  "pipeline.evaluations": "1500000",
  "pipeline.path.full": "1500000",
  "pipeline.path.fast": "0",
  "pipeline.path.metrics_only": "0",
  "event_bus.emits": "0",
  "event_bus.deliveries": "0",
  "event_bus.interest_checks": "4364312",
  "event_bus.interest_hits": "0",
  "manager.event_received": "0",
  "manager.event_enqueue_attempts": "0",
  "manager.projection_builds": "0",
  "pipeline.event.operation_started": "0",
  "pipeline.event.operation_completed": "0",
  "runtime.gc.heap.allocs.bytes": "6338305520",
  "runtime.gc.heap.allocs.objects": "93664027"
}
```

Prometheus+instrumentation `gocache-ipc-otel` pipelined benchstats selected counters:

```json
{
  "pipeline.evaluations": "1500000",
  "pipeline.path.full": "1500000",
  "pipeline.path.fast": "0",
  "pipeline.path.metrics_only": "0",
  "event_bus.emits": "2132764",
  "event_bus.deliveries": "2132764",
  "event_bus.interest_checks": "5760640",
  "event_bus.interest_hits": "3630320",
  "manager.event_received": "2132764",
  "manager.event_enqueue_attempts": "2132764",
  "manager.projection_builds": "880303",
  "pipeline.event.operation_started": "1065160",
  "pipeline.event.operation_completed": "1065160",
  "runtime.gc.heap.allocs.bytes": "17970120528",
  "runtime.gc.heap.allocs.objects": "299383066"
}
```

## Interpretation guardrails

- Core is down versus the older `bench-discovered-fixes-baseline` capture, but the current Valkey control is also down strongly versus the same historical baseline, especially in pipelined workloads. That means host/environment drift is a real contributor and the core delta should not be attributed solely to the telemetry migration from this single comparison.
- Current Prometheus-only IPC is very close to current core in pipelined mode for most commands, which suggests the new telemetry-manager plumbing did not by itself recreate the old Prometheus IPC cliff.
- Current Prometheus+instrumentation remains much slower than current Prometheus-only IPC in pipelined mode, so runtime event/instrumentation projection is still the dominant modular overhead in that target.
- The historical `gocache-ipc-otel` pipelined scenario improved directionally versus `runtime-otlp-benchstats-20260531`, but this is a single-run historical comparison and should be repeated before thesis-grade claims.
- `BENCH_STATS=1` is an attribution run; use it to diagnose, not as the clean headline performance number.

## Variant vs Valkey tables

See `variant-valkey-benchstats-comparison.md` for the requested standard, pipelined, and previous-benchstats comparison tables.
