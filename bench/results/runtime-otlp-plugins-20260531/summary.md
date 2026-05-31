# Runtime OTLP Prometheus + Instrumentation Benchmark — 2026-05-31

## Scope

Measured GoCache runtime OTLP overhead against the existing containerized `valkey-benchmark` harness.

Targets:

- `runtime-otlp-valkey`: `valkey/valkey:8`, persistence disabled.
- `runtime-otlp-gocache-ipc`: GoCache with the `prometheus` IPC plugin loaded.
- `runtime-otlp-gocache-ipc-otel`: GoCache with `prometheus` + `instrumentation` IPC plugins loaded; `instrumentation` exports OTLP traces/logs to a local `otel/opentelemetry-collector-contrib:latest` collector configured with `nop` exporters.

Harness parameters:

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
RESULTS_DIR="$PWD/bench/results/runtime-otlp-plugins-20260531" \
  GIT_MASTER=1 bash bench/redis-benchmark/run.sh runtime-otlp --target valkey

RESULTS_DIR="$PWD/bench/results/runtime-otlp-plugins-20260531" \
  REBUILD=1 GOCACHE_IPC_IMAGE=gocache-bench:runtime-otlp-prom \
  IPC_PLUGINS=prometheus \
  GIT_MASTER=1 bash bench/redis-benchmark/run-ipc.sh runtime-otlp --target gocache-ipc

RESULTS_DIR="$PWD/bench/results/runtime-otlp-plugins-20260531" \
  REBUILD=1 GOCACHE_IPC_IMAGE=gocache-bench:runtime-otlp-otel \
  GIT_MASTER=1 bash bench/redis-benchmark/run-ipc.sh runtime-otlp --target gocache-ipc-otel
```

Comparison commands:

```bash
RESULTS_DIR="$PWD/bench/results/runtime-otlp-plugins-20260531" \
  GIT_MASTER=1 bash bench/redis-benchmark/compare.sh runtime-otlp-valkey runtime-otlp-gocache-ipc-otel

RESULTS_DIR="$PWD/bench/results/runtime-otlp-plugins-20260531" \
  GIT_MASTER=1 bash bench/redis-benchmark/compare.sh runtime-otlp-gocache-ipc runtime-otlp-gocache-ipc-otel
```

## Aggregate throughput results

| Comparison | Suite | Total RPS ratio | Total RPS delta | Geomean RPS ratio | Geomean delta | Median RPS ratio | Median delta |
|---|---:|---:|---:|---:|---:|---:|---:|
| Valkey -> GoCache+Prometheus | standard | `1.1097x` | `+11.0%` | `1.1117x` | `+11.2%` | `1.1350x` | `+13.5%` |
| Valkey -> GoCache+Prometheus | pipelined | `0.9539x` | `-4.6%` | `0.9623x` | `-3.8%` | `0.9706x` | `-2.9%` |
| Valkey -> GoCache+Prometheus+Instrumentation | standard | `0.8636x` | `-13.6%` | `0.8630x` | `-13.7%` | `0.8697x` | `-13.0%` |
| Valkey -> GoCache+Prometheus+Instrumentation | pipelined | `0.2886x` | `-71.1%` | `0.2991x` | `-70.1%` | `0.2702x` | `-73.0%` |
| GoCache+Prometheus -> GoCache+Prometheus+Instrumentation | standard | `0.7783x` | `-22.2%` | `0.7763x` | `-22.4%` | `0.7923x` | `-20.8%` |
| GoCache+Prometheus -> GoCache+Prometheus+Instrumentation | pipelined | `0.3025x` | `-69.7%` | `0.3109x` | `-68.9%` | `0.3093x` | `-69.1%` |

## Worst command ratios

Against Valkey, `gocache-ipc-otel` worst RPS ratios:

- Standard: `SADD 0.768x`, `RPOP 0.791x`, `LPOP 0.805x`, `MSET 0.814x`, `RPUSH 0.838x`.
- Pipelined: `RPUSH 0.203x`, `LPUSH 0.204x`, `SADD 0.225x`, `HSET 0.226x`, `LPUSH setup 0.227x`.

Against Prometheus-only GoCache, incremental instrumentation ratios:

- Standard: worst `RPOP 0.692x`, `MSET 0.700x`, `LPOP 0.710x`, `LRANGE_100 0.748x`, `RPUSH 0.749x`.
- Pipelined: worst `INCR 0.222x`, `SET 0.234x`, `RPUSH 0.256x`, `LPUSH 0.257x`, `LPUSH setup 0.267x`.

## Memory

| Target | Baseline RSS | Post-standard RSS | Final RSS | Delta RSS |
|---|---:|---:|---:|---:|
| Valkey | `9,215,934` | `23,163,043` | `28,531,752` | `19,315,818` |
| GoCache+Prometheus | `43,473,960` | `151,309,516` | `184,129,945` | `140,655,985` |
| GoCache+Prometheus+Instrumentation | `48,517,611` | `259,522,560` | `295,908,147` | `247,390,536` |
| OTLP collector sidecar | `38,094,766` | `38,094,766` | `38,094,766` | `0` |

Incremental target-container RSS for instrumentation over Prometheus-only:

- Baseline: `+5,043,651` bytes.
- Post-standard: `+108,213,044` bytes.
- Final: `+111,778,202` bytes.
- Delta RSS: `+106,734,551` bytes.

## Interpretation

The standard non-pipelined suite keeps GoCache+Prometheus+Instrumentation within roughly a 14% total/geomean throughput regression versus Valkey, but the pipelined suite regresses heavily: roughly 70-73% versus Valkey and roughly 69% versus Prometheus-only GoCache.

That indicates the current runtime instrumentation path is not performance-acceptable for pipelined workloads under the ADR-0022 <=20% modular overhead budget. The next optimization should focus on reducing per-operation event/exporter work for pipelined command bursts: sampling, batching span/log export more aggressively, interest/detail gates, or benchmark-gated event traffic class work.

## Caveats

- Single run per target; repeat runs are needed before thesis-grade final claims.
- `valkey-benchmark` emitted `WARNING: Could not fetch server CONFIG` during GoCache runs, but completed standard and pipelined CSV output.
- Docker build used the legacy builder because the local `docker-buildx` plugin is missing.
- The filesystem was initially full; unused Docker images/stopped containers were pruned before the successful run.
- Results are from the dirty branch `feat/runtime-otlp-traces-logs` at `2c69deb263c49da405db150ce8a56137d94c1b78` plus uncommitted runtime OTLP changes included in the Docker build context.
