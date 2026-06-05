# OperationTracker Telemetry BENCH_STATS Attribution — 2026-06-05

## Scope

Measured the post-OperationTracker telemetry migration IPC attribution path with the same `BENCH_STATS=1` style used by `bench/results/runtime-otlp-benchstats-20260531/summary.md`. The goal is diagnosis: compare Prometheus-only IPC against Prometheus+runtime instrumentation IPC, then compare current attribution counters against the previous `runtime-otlp-benchstats-20260531` capture.

Targets:

- `operationtracker-telemetry-post-gocache-ipc`: GoCache with `prometheus` + benchmark-only `benchprobe` IPC plugins.
- `operationtracker-telemetry-post-gocache-ipc-otel`: GoCache with `prometheus` + runtime `instrumentation` + benchmark-only `benchprobe` IPC plugins; `instrumentation` exports OTLP traces/logs to a local `otel/opentelemetry-collector-contrib:latest` collector configured by the harness.

Harness parameters:

- `BENCH_STATS=1`
- `BENCH_N=100000`
- `BENCH_CLIENTS=50`
- `BENCH_KEYSPACE=100000`
- `BENCH_PIPELINE=10`
- `BENCH_TARGET_CPUS=0-3`
- `BENCH_CLIENT_CPUS=4-7`
- `BENCH_MEM_LIMIT=2g`
- suite: `ping_inline,ping_mbulk,set,get,incr,lpush,rpush,lpop,rpop,sadd,hset,spop,lrange_100,mset`

## Commands

```bash
RESULTS_DIR="$PWD/bench/results/zero-allocation-telemetry-benchstats-20260605" \
  BENCH_STATS=1 REBUILD=1 \
  GOCACHE_IPC_IMAGE=gocache-bench:operationtracker-telemetry-post-prom \
  IPC_PLUGINS=prometheus \
  GIT_MASTER=1 ./bench/redis-benchmark/run-ipc.sh operationtracker-telemetry-post --target gocache-ipc

RESULTS_DIR="$PWD/bench/results/zero-allocation-telemetry-benchstats-20260605" \
  BENCH_STATS=1 REBUILD=1 \
  GOCACHE_IPC_IMAGE=gocache-bench:zero-allocation-telemetry-benchstats-20260605 \
  GIT_MASTER=1 ./bench/redis-benchmark/run-ipc.sh operationtracker-telemetry-post --target gocache-ipc-otel
```

Primary raw outputs live in `bench/results/zero-allocation-telemetry-benchstats-20260605/`.

## Aggregate throughput results

Comparison is incremental instrumentation cost: Prometheus-only GoCache to Prometheus+runtime instrumentation GoCache, both with `BENCH_STATS=1` and `benchprobe` present.

| Suite | Total RPS ratio | Total RPS delta | Geomean RPS ratio | Geomean RPS delta | Geomean p99 latency ratio | Geomean p99 delta |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| standard | 0.8645x | -13.5% | 0.8648x | -13.5% | 2.2514x | +125.1% |
| pipelined | 0.5522x | -44.8% | 0.5328x | -46.7% | 1.9885x | +98.9% |

Worst per-command RPS deltas:

| Suite | Command | Prometheus-only RPS | Prometheus+Instrumentation RPS | Ratio | Delta |
| --- | ---: | ---: | ---: | ---: | ---: |
| standard | `MSET (10 keys)` | 97,276 | 77,580 | 0.798x | -20.2% |
| standard | `HSET` | 99,305 | 81,235 | 0.818x | -18.2% |
| standard | `INCR` | 99,108 | 81,566 | 0.823x | -17.7% |
| standard | `PING_MBULK` | 101,937 | 84,104 | 0.825x | -17.5% |
| standard | `GET` | 96,899 | 81,433 | 0.840x | -16.0% |
| pipelined | `SET` | 671,141 | 274,725 | 0.409x | -59.1% |
| pipelined | `INCR` | 628,931 | 264,550 | 0.421x | -57.9% |
| pipelined | `RPOP` | 505,050 | 221,729 | 0.439x | -56.1% |
| pipelined | `LPOP` | 487,805 | 227,273 | 0.466x | -53.4% |
| pipelined | `LPUSH` | 458,716 | 220,751 | 0.481x | -51.9% |

Worst per-command p99 latency increases:

| Suite | Command | Prometheus-only p99 ms | Prometheus+Instrumentation p99 ms | Ratio | Delta |
| --- | ---: | ---: | ---: | ---: | ---: |
| standard | `PING_MBULK` | 0.687 | 3.455 | 5.03x | +402.9% |
| standard | `HSET` | 0.911 | 3.303 | 3.63x | +262.6% |
| standard | `LPUSH` | 0.943 | 3.303 | 3.50x | +250.3% |
| standard | `LPOP` | 0.983 | 3.143 | 3.20x | +219.7% |
| standard | `PING_INLINE` | 1.159 | 3.471 | 2.99x | +199.5% |
| pipelined | `GET` | 2.591 | 9.479 | 3.66x | +265.8% |
| pipelined | `PING_MBULK` | 2.023 | 5.391 | 2.66x | +166.5% |
| pipelined | `INCR` | 5.591 | 12.439 | 2.22x | +122.5% |
| pipelined | `SET` | 4.999 | 10.975 | 2.20x | +119.5% |
| pipelined | `LPUSH` | 4.359 | 9.367 | 2.15x | +114.9% |

## BENCH_STATS attribution — current run

Prometheus-only no longer reports the previous `metrics_only` path in this branch: all `1,500,000` command evaluations are on `pipeline.path.full`, but it still records no operation events, no context snapshots, no manager bridge runs, and no projections. That means the always-operation pipeline bookkeeping is present, while runtime instrumentation event/projection work is still gated off without the instrumentation subscriber.

Prometheus+instrumentation current run:

| Metric | Standard window | Pipelined window | Notes |
| --- | ---: | ---: | ---: |
| `pipeline.evaluations` | 1,500,000 | 1,500,000 | command sink evaluations |
| `pipeline.path.full` | 1,500,000 | 1,500,000 | full operation path |
| `pipeline.context_snapshots` | 0 | 0 | context snapshots |
| `event_bus.emits` | 3,002,916 | 2,132,764 | operation/event emits |
| `event_bus.deliveries` | 3,002,916 | 2,132,764 | subscriber deliveries |
| `manager.bridge_handler_runs` | 3,002,916 | 2,132,764 | plugin manager bridge callbacks |
| `manager.event_enqueue_attempts` | 3,002,916 | 2,132,764 | event handoff attempts |
| `manager.projection_builds` | 2,495,132 | 880,303 | GCPC event projections |

Average measured producer-side cost for Prometheus+instrumentation:

| Probe | Standard avg | Pipelined avg |
| --- | ---: | ---: |
| context snapshot | 0 ns | 0 ns |
| operation started event build | 223 ns | 233 ns |
| operation completed event build | 295 ns | 250 ns |
| event-bus emit | 0.636 µs | 0.688 µs |
| event-bus delivery | 453 ns | 487 ns |
| manager bridge handler | 365 ns | 398 ns |
| manager enqueue handoff | 274 ns | 301 ns |
| manager projection | 690 ns | 847 ns |

Runtime counters in the current run:

| Runtime metric | Prometheus-only standard | Prometheus-only pipelined | Instrumentation standard | Instrumentation pipelined |
| --- | ---: | ---: | ---: | ---: |
| `runtime.gc.heap.allocs.bytes` | 3.2 GiB | 5.9 GiB | 10.3 GiB | 16.7 GiB |
| `runtime.gc.heap.allocs.objects` | 52,307,128 | 93,664,027 | 189,443,489 | 299,383,066 |
| `runtime.sync.mutex.wait.total.seconds` | 2.340s | 102.475s | 13.720s | 203.763s |
| `runtime.memory.classes.heap.objects.bytes` | 126.9 MiB | 94.1 MiB | 88.8 MiB | 101.2 MiB |
| `runtime.sched.goroutines.goroutines` | 25 | 25 | 31 | 31 |

## Current vs previous BENCH_STATS comparison

Previous baseline: `bench/results/runtime-otlp-benchstats-20260531/`. Current: `bench/results/zero-allocation-telemetry-benchstats-20260605/`.

| Target | Window | Avg RPS vs previous | metrics_only prev -> current | full prev -> current | context snapshots prev -> current | event emits prev -> current | projections prev -> current | alloc bytes prev -> current | alloc objects prev -> current | mutex wait prev -> current | RSS prev -> current |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Prometheus-only IPC | standard | -3.5% | 1,500,000 -> 0 (-100.0%) | 0 -> 1,500,000 (N/A) | 0 -> 0 (N/A) | 0 -> 0 (N/A) | 0 -> 0 (N/A) | 2.2 GiB -> 3.2 GiB (+43.4%) | 23,764,916 -> 52,307,128 (+120.1%) | 1.327s -> 2.340s (+76.4%) | 160.2 MiB -> 193.4 MiB (+20.7%) |
| Prometheus-only IPC | pipelined | -24.7% | 1,500,000 -> 0 (-100.0%) | 0 -> 1,500,000 (N/A) | 0 -> 0 (N/A) | 0 -> 0 (N/A) | 0 -> 0 (N/A) | 4.5 GiB -> 5.9 GiB (+31.2%) | 48,613,154 -> 93,664,027 (+92.7%) | 72.983s -> 102.475s (+40.4%) | 185.9 MiB -> 218.4 MiB (+17.5%) |
| Prometheus+instrumentation IPC | standard | -4.3% | 0 -> 0 (N/A) | 1,500,000 -> 1,500,000 (+0.0%) | 3,000,000 -> 0 (-100.0%) | 3,000,000 -> 3,002,916 (+0.1%) | 2,927,292 -> 2,495,132 (-14.8%) | 9.0 GiB -> 10.3 GiB (+14.6%) | 150,341,198 -> 189,443,489 (+26.0%) | 18.601s -> 13.720s (-26.2%) | 221.5 MiB -> 251.9 MiB (+13.7%) |
| Prometheus+instrumentation IPC | pipelined | +26.6% | 0 -> 0 (N/A) | 1,500,000 -> 1,500,000 (+0.0%) | 3,000,000 -> 0 (-100.0%) | 3,000,000 -> 2,132,764 (-28.9%) | 1,258,138 -> 880,303 (-30.0%) | 15.7 GiB -> 16.7 GiB (+6.6%) | 252,286,829 -> 299,383,066 (+18.7%) | 246.548s -> 203.763s (-17.4%) | 222.8 MiB -> 296.5 MiB (+33.1%) |

## IPC queue/write attribution

`plugin_ipc` counters are monotonic per plugin connection, so standard and pipelined attribution below is computed as window deltas for `instrumentation.*`.

| Run | Window | send attempts | accepted sends | fire-and-forget drops | drop rate | write attempts | write batches | envelopes per batch | write latency per envelope | queue lag per envelope | enqueue latency per attempt |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| previous | standard | 3,000,000 | 2,927,292 | 72,708 | 2.42% | 2,927,292 | 91,954 | 31.83 | 3.465 µs | 1.437 ms | 1.842 µs |
| previous | pipelined | 3,000,000 | 1,258,138 | 1,741,862 | 58.06% | 1,258,138 | 39,330 | 31.99 | 5.367 µs | 5.893 ms | 1.598 µs |
| current | standard | 3,002,916 | 2,495,132 | 507,784 | 16.91% | 2,495,132 | 78,382 | 31.83 | 4.855 µs | 3.476 ms | 0.143 µs |
| current | pipelined | 2,132,764 | 880,303 | 1,252,461 | 58.72% | 880,303 | 27,522 | 31.99 | 6.263 µs | 6.237 ms | 0.168 µs |

## Memory samples

| Target | Baseline RSS | Post-standard RSS | Final RSS | Delta RSS |
| --- | ---: | ---: | ---: | ---: |
| Current Prometheus+benchprobe | 58.8 MiB | 193.4 MiB | 218.4 MiB | 159.6 MiB |
| Current Prometheus+Instrumentation+benchprobe | 63.2 MiB | 251.9 MiB | 296.5 MiB | 233.3 MiB |
| Current OTLP collector | 196.9 MiB | 196.9 MiB | 196.9 MiB | 0.0 B |
| Previous Prometheus+benchprobe | 46.2 MiB | 160.2 MiB | 185.9 MiB | 139.7 MiB |
| Previous Prometheus+Instrumentation+benchprobe | 50.8 MiB | 221.5 MiB | 222.8 MiB | 172.0 MiB |
| Previous OTLP collector | 40.5 MiB | 40.5 MiB | 40.5 MiB | -10.2 KiB |

Incremental current target-container RSS for instrumentation over Prometheus-only:

- Baseline: `4.4 MiB`.
- Post-standard: `58.5 MiB`.
- Final: `78.1 MiB`.
- Delta RSS: `73.7 MiB`.

## Interpretation

The post-migration benchmark changed the Prometheus-only path shape: it moved from the previous `metrics_only` pipeline path to `full` pipeline path, increasing allocation counters and mutex wait. Despite that, Prometheus-only throughput stayed relatively close to the current core run and the instrumentation gap is still the larger problem.

The instrumentation path improved materially versus the previous pipelined `gocache-ipc-otel` run: geomean/average throughput is up, event emissions dropped from 3.0M to ~2.13M, projections dropped from ~1.26M to ~880K, and mutex wait fell from ~246.5s to ~203.8s in the pipelined attribution snapshot. However, allocation objects increased from ~252M to ~299M and target RSS rose from ~222.8 MiB to ~296.5 MiB. So the operation/context snapshot removal helped event volume and mutex wait, but current projection/allocation/RSS pressure remains high.

The current `gocache-ipc-otel` pipelined run still spends the command window producing/delivering ~2.13M events and building ~880K projections, with ~16.7 GiB runtime alloc bytes and ~299M alloc objects reported by runtime metrics. That is the next optimization target: reduce per-event allocation/projection and avoid doing work for events that will be dropped or not needed by a subscriber.

## Caveats

- Single run per target; repeat runs are needed before final thesis-grade numbers.
- `BENCH_STATS=1` adds diagnostic counters and should be used for attribution, not normal headline performance claims.
- `plugin_ipc` counters are monotonic per connection, so IPC window values are computed by subtracting baseline/standard snapshots.
- Runtime allocation metrics are process runtime snapshots, not reset benchmark-window counters.
- GoCache runs emitted `WARNING: Could not fetch server CONFIG`; CSV, memory, config, and benchstats artifacts were still written.
- Docker emitted the local non-fatal `docker-buildx` plugin warning and used the legacy builder.
