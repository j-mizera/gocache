# Runtime OTLP BENCH_STATS Attribution — 2026-05-31

## Scope

Measured the runtime OTLP instrumentation overhead with the `BENCH_STATS=1` attribution harness added for this branch. The goal was diagnosis, not optimization: preserve full runtime traces/logs observability and identify where the Prometheus+instrumentation path spends time compared with the Prometheus-only IPC baseline.

Targets:

- `runtime-otlp-benchstats-gocache-ipc`: GoCache with `prometheus` + benchmark-only `benchprobe` IPC plugins.
- `runtime-otlp-benchstats-gocache-ipc-otel`: GoCache with `prometheus` + runtime `instrumentation` + benchmark-only `benchprobe` IPC plugins; `instrumentation` exports OTLP traces/logs to a local `otel/opentelemetry-collector-contrib:latest` collector configured by the harness.

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
RESULTS_DIR="$PWD/bench/results/runtime-otlp-benchstats-20260531" \
  BENCH_STATS=1 REBUILD=1 \
  GOCACHE_IPC_IMAGE=gocache-bench:runtime-otlp-benchstats-prom \
  IPC_PLUGINS=prometheus \
  GIT_MASTER=1 ./bench/redis-benchmark/run-ipc.sh runtime-otlp-benchstats --target gocache-ipc

RESULTS_DIR="$PWD/bench/results/runtime-otlp-benchstats-20260531" \
  BENCH_STATS=1 REBUILD=1 \
  GOCACHE_IPC_IMAGE=gocache-bench:runtime-otlp-benchstats-otel \
  GIT_MASTER=1 ./bench/redis-benchmark/run-ipc.sh runtime-otlp-benchstats --target gocache-ipc-otel
```

Primary raw outputs:

- `runtime-otlp-benchstats-gocache-ipc.csv`
- `runtime-otlp-benchstats-gocache-ipc-pipelined.csv`
- `runtime-otlp-benchstats-gocache-ipc-otel.csv`
- `runtime-otlp-benchstats-gocache-ipc-otel-pipelined.csv`
- `runtime-otlp-benchstats-gocache-ipc-*.json`
- `runtime-otlp-benchstats-gocache-ipc-otel-*.json`
- `runtime-otlp-benchstats-gocache-ipc-memory.txt`
- `runtime-otlp-benchstats-gocache-ipc-otel-memory.txt`

## Aggregate throughput results

Comparison is incremental instrumentation cost: Prometheus-only GoCache to Prometheus+runtime instrumentation GoCache, both with `BENCH_STATS=1` and `benchprobe` present.

| Suite | Total RPS ratio | Total RPS delta | Geomean RPS ratio | Geomean RPS delta | Geomean p99 latency ratio | Geomean p99 delta |
|---|---:|---:|---:|---:|---:|---:|
| standard | `0.8731x` | `-12.7%` | `0.8726x` | `-12.7%` | `3.6434x` | `+264.3%` |
| pipelined | `0.3112x` | `-68.9%` | `0.3195x` | `-68.0%` | `3.6568x` | `+265.7%` |

Worst per-command RPS deltas:

| Suite | Command | Prometheus-only RPS | Prometheus+Instrumentation RPS | Ratio | Delta |
|---|---|---:|---:|---:|---:|
| standard | `HSET` | `104058` | `81699` | `0.785x` | `-21.5%` |
| standard | `RPUSH` | `103413` | `85397` | `0.826x` | `-17.4%` |
| standard | `MSET (10 keys)` | `92937` | `78247` | `0.842x` | `-15.8%` |
| standard | `RPOP` | `104058` | `89047` | `0.856x` | `-14.4%` |
| standard | `GET` | `102987` | `89047` | `0.865x` | `-13.5%` |
| pipelined | `LPOP` | `735294` | `188324` | `0.256x` | `-74.4%` |
| pipelined | `INCR` | `877193` | `228310` | `0.260x` | `-74.0%` |
| pipelined | `LPUSH` | `724638` | `192678` | `0.266x` | `-73.4%` |
| pipelined | `SADD` | `662252` | `178571` | `0.270x` | `-73.0%` |
| pipelined | `RPUSH` | `628931` | `170358` | `0.271x` | `-72.9%` |

Worst per-command p99 latency increases:

| Suite | Command | Prometheus-only p99 ms | Prometheus+Instrumentation p99 ms | Ratio | Delta |
|---|---|---:|---:|---:|---:|
| standard | `HSET` | `0.583` | `3.079` | `5.28x` | `+428.1%` |
| standard | `SPOP` | `0.415` | `2.191` | `5.28x` | `+428.0%` |
| standard | `PING_MBULK` | `0.423` | `2.231` | `5.27x` | `+427.4%` |
| pipelined | `PING_INLINE` | `0.479` | `11.855` | `24.75x` | `+2374.9%` |
| pipelined | `PING_MBULK` | `0.831` | `6.311` | `7.59x` | `+659.4%` |
| pipelined | `LPOP` | `2.015` | `8.311` | `4.12x` | `+312.5%` |

## BENCH_STATS attribution

`bench.stats` snapshots are reset between benchmark windows, so these numbers describe each standard or pipelined window directly. Prometheus-only confirms the fast path: all `1,500,000` command evaluations stayed on `pipeline.path.metrics_only`; no operation events, context snapshots, manager bridge runs, or projections were recorded.

Prometheus+instrumentation forces the full operation lifecycle path for every command:

| Metric | Standard window | Pipelined window | Notes |
|---|---:|---:|---|
| `pipeline.evaluations` | `1,500,000` | `1,500,000` | command sink evaluations |
| `pipeline.path.full` | `1,500,000` | `1,500,000` | all commands used full sink path |
| `pipeline.context_snapshots` | `3,000,000` | `3,000,000` | start + completion snapshots |
| `event_bus.emits` | `3,000,000` | `3,000,000` | operation start + completion events |
| `event_bus.deliveries` | `3,000,000` | `3,000,000` | one runtime instrumentation subscriber |
| `manager.bridge_handler_runs` | `3,000,000` | `3,000,000` | plugin manager event bridge callbacks |
| `manager.event_enqueue_attempts` | `3,000,000` | `3,000,000` | event handoff attempts |
| `manager.projection_builds` | `2,927,292` | `1,258,138` | only accepted IPC events are projected |

Average measured producer-side cost:

| Probe | Standard avg | Pipelined avg | Interpretation |
|---|---:|---:|---|
| context snapshot | `440 ns/snapshot` | `413 ns/snapshot` | paid twice per command |
| operation event construction | `827 ns/event` | `809 ns/event` | event struct/detail construction |
| event-bus emit | `2.997 µs/emit` | `3.323 µs/emit` | includes delivery path work |
| event-bus delivery | `2.254 µs/delivery` | `2.040 µs/delivery` | subscriber callback cost as measured by bus |
| manager bridge handler | `2.163 µs/event` | `1.948 µs/event` | manager-side callback wrapper |
| manager enqueue handoff | `2.058 µs/event` | `1.843 µs/event` | lazy send/enqueue attempt path |
| manager projection | `616 ns/projected event` | `703 ns/projected event` | plugin-specific GCPC event projection |

Runtime allocation snapshots also diverged sharply with instrumentation enabled:

| Runtime metric | Prometheus-only standard | Instrumentation standard | Prometheus-only pipelined | Instrumentation pipelined |
|---|---:|---:|---:|---:|
| `runtime.gc.heap.allocs.bytes` | `2,405,961,160` | `9,694,034,928` | `4,831,674,272` | `16,853,783,424` |
| `runtime.gc.heap.allocs.objects` | `23,764,916` | `150,341,198` | `48,613,154` | `252,286,829` |

These runtime allocation values are process runtime snapshots rather than reset window counters, but the direction is clear: the runtime instrumentation path adds a large amount of allocation pressure before any optimization has started.

## IPC queue/write attribution

`plugin.ipc` counters are monotonic per plugin connection, so standard and pipelined attribution below is computed as window deltas for `instrumentation.*`.

| Metric | Standard window | Pipelined window |
|---|---:|---:|
| send attempts | `3,000,000` | `3,000,000` |
| accepted sends | `2,927,292` | `1,258,138` |
| fire-and-forget drops | `72,708` | `1,741,862` |
| drop rate | `2.42%` | `58.06%` |
| write attempts/envelopes | `2,927,292` | `1,258,138` |
| write batches | `91,954` | `39,330` |
| envelopes per batch | `31.83` | `31.99` |
| write latency per envelope | `3.465 µs` | `5.367 µs` |
| queue lag per envelope | `1.437 ms` | `5.893 ms` |
| enqueue latency per attempt | `1.842 µs` | `1.598 µs` |

The writer batches are already near the configured 32-envelope batch size, but pipelined workloads still overwhelm the IPC instrumentation path. The result is not merely slow export; it is backpressure/drop behavior inside the best-effort event stream while the command path still pays full producer-side event/context work for every command.

## Memory samples

| Target | Baseline RSS | Post-standard RSS | Final RSS | Delta RSS |
|---|---:|---:|---:|---:|
| GoCache+Prometheus+benchprobe | `48,465,182` | `167,981,875` | `194,930,278` | `146,465,096` |
| GoCache+Prometheus+Instrumentation+benchprobe | `53,257,175` | `232,259,584` | `233,622,732` | `180,365,557` |
| OTLP collector sidecar | `42,498,785` | `42,488,299` | `42,488,299` | `-10,486` |

Incremental target-container RSS for instrumentation over Prometheus-only in this run:

- Baseline: `+4,791,993` bytes.
- Post-standard: `+64,277,709` bytes.
- Final: `+38,692,454` bytes.
- Delta RSS: `+33,900,461` bytes.

## Interpretation

The new attribution confirms that the severe pipelined regression is not explained by Prometheus metrics, because Prometheus-only stays on the metrics-only query/pull path and records no operation-event work. Runtime instrumentation makes every command emit two operation lifecycle events, two context snapshots, bus delivery, manager enqueue attempts, projection for accepted events, and IPC/OTLP plugin work.

The biggest measured client-visible problem is pipelined saturation: throughput drops by about `68%` geomean and `69%` total RPS versus Prometheus-only, while p99 latency grows about `3.66x` geomean. The IPC writer is already batching near 32 envelopes per write, yet the instrumentation plugin accepts only `41.9%` of pipelined event attempts and drops `58.1%`; accepted envelopes wait about `5.89 ms` on average before write.

The producer side remains important because drops do not avoid upstream work. Even when pipelined events are dropped later, the command path still pays context snapshot, event construction, event-bus delivery, bridge callback, enqueue attempt, and often allocation costs. That makes producer-side compact operation summaries / lazy detail projection a stronger next optimization candidate than only tuning the existing writer batch size.

A data-backed next optimization lane should compare at least these prototypes before choosing an ADR-level direction:

1. Compact typed operation summaries that avoid full `ContextSnapshot` and high-cardinality detail allocation on the command path.
2. Lazy per-plugin projection so dropped best-effort events do not build GCPC projections.
3. Larger or adaptive traffic-class batching only if the queue/write path remains dominant after producer-side work is reduced.
4. Plugin-side OTel conversion/export attribution if producer and IPC changes still leave the plugin process as the limiting stage.

## Caveats

- Single run per target; repeat runs are needed before final thesis-grade numbers.
- `BENCH_STATS=1` adds diagnostic counters and should be used for attribution, not normal headline performance claims.
- `plugin.ipc` counters are monotonic per connection, so IPC window values are computed by subtracting baseline/standard snapshots.
- Runtime allocation metrics are process runtime snapshots, not reset benchmark-window counters.
- Docker emitted the local non-fatal `docker-buildx` plugin warning and used the legacy builder.
- Results are from dirty branch `feat/runtime-otlp-traces-logs` at `2c69deb263c49da405db150ce8a56137d94c1b78` plus uncommitted BENCH_STATS/runtime OTLP changes included in the Docker build context.
