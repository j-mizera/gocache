# Containerized redis-benchmark comparison harness

End-to-end throughput + latency comparison for **gocache vs valkey** under
identical conditions: both the target server and the `valkey-benchmark`
client run in Docker containers on a shared bridge network, with matching
CPU/memory limits, so the numbers are apples-to-apples for the thesis.

Valkey is the BSD-licensed community fork of Redis. `valkey-benchmark` is
`redis-benchmark` under a different name — the CSV output is byte-identical.

## Prerequisites

- Docker Engine (tested against v28.3 / 29.4)
- Docker-group membership for the current user. If the daemon returns
  `permission denied`, run `newgrp docker` in the shell once, or prefix the
  runner with `sg docker -c "bench/redis-benchmark/run.sh …"`.

The harness pulls `valkey/valkey:8` on first use (~50 MB) and builds the
gocache image locally from the repo's root `Dockerfile` with `PLUGINS=""`
(no embedded observability — lean binary for fair comparison). IPC-plugin
runs use `bench/redis-benchmark/Dockerfile.ipc`, which keeps the production
image unchanged while adding selected plugin binaries to the benchmark image.

## Usage

```bash
# Baseline run on the current branch — capture both sides.
./bench/redis-benchmark/run.sh resp-pool --target gocache
./bench/redis-benchmark/run.sh resp-pool --target valkey

# IPC-plugin overhead run with the Prometheus metrics plugin. The harness
# records metrics.telemetry JSON snapshots around the standard and pipelined suites.
./bench/redis-benchmark/run-ipc.sh resp-pool --target gocache-ipc

# Optional benchstats run: enables in-process counter collection for IPC consumers.
BENCH_STATS=1 ./bench/redis-benchmark/run-ipc.sh resp-pool --target gocache-ipc-otel

# Pub/Sub fanout runs. These use real SUBSCRIBE connections and verify every
# published message reaches every subscriber.
./bench/redis-benchmark/run-pubsub.sh resp-pool --target valkey
./bench/redis-benchmark/run-pubsub.sh resp-pool --target gocache-pubsub

# Full four-way request/response matrix.
./bench/redis-benchmark/run-matrix.sh resp-pool

# After gc-opaque-index lands:
./bench/redis-benchmark/run-matrix.sh gc-opaque-index

# Compare.
./bench/redis-benchmark/compare.sh resp-pool-gocache resp-pool-valkey
./bench/redis-benchmark/compare.sh resp-pool-gocache resp-pool-gocache-ipc
./bench/redis-benchmark/compare-pubsub.sh resp-pool-valkey resp-pool-gocache-pubsub
./bench/redis-benchmark/compare.sh resp-pool-gocache gc-opaque-index-gocache
```

Output per run lands under `bench/results/<branch>/` (the script derives `<branch>` from `git rev-parse --abbrev-ref HEAD` with `/` rewritten to `-`; override with `RESULTS_DIR=`):

- `<label>-<target>.csv` — standard suite (no pipelining)
- `<label>-<target>-pipelined.csv` — same suite with `-P 10`
- `<label>-<target>-memory.txt` — container RSS before/after + run metadata
- `<label>-<target>-config.yaml` — generated only for IPC targets, preserving the exact plugin config used for the capture
- `<label>-<target>-telemetry-baseline.json`, `<label>-<target>-telemetry-standard.json`, `<label>-<target>-telemetry-pipelined.json` — IPC-only diagnostic snapshots fetched from the Prometheus plugin `/telemetry` endpoint, backed by `server:query:metrics.telemetry`

Targets now form the benchmark matrix:

| Target | Script | What it measures |
|--------|--------|------------------|
| `valkey` | `run.sh` | Reference Valkey server with persistence disabled |
| `gocache` | `run.sh` | Core GoCache with no IPC plugins |
| `gocache-ipc` | `run-ipc.sh` | GoCache with the `prometheus` IPC plugin registered, Prometheus metrics scraped from `server:query:metrics.commands` aggregate snapshots, plus diagnostic `metrics.telemetry` JSON snapshots |
| `gocache-ipc-otel` | `run-ipc.sh` | GoCache with `prometheus` plus runtime `instrumentation` traces/logs exporting to a local OpenTelemetry Collector `nop` pipeline |
| `gocache-pubsub` | `run-pubsub.sh` | GoCache with the `pubsub` IPC plugin, measured with real subscribers and `ClientPushV1` fanout |

Pub/Sub output is separate from the generic matrix:

- `<label>-<target>-pubsub.csv` — `PUBLISH_fanout_<N>` rows with publisher rps/p50/p99 plus verified delivery counts
- `<label>-<target>-pubsub-memory.txt` — target RSS before/after the Pub/Sub workload
- `<label>-gocache-pubsub-config.yaml` — exact plugin config for GoCache Pub/Sub captures

## What the suite measures

Standard `valkey-benchmark` command set:

```
ping_inline, ping_mbulk, set, get, incr, lpush, rpush, lpop, rpop,
sadd, hset, spop, lrange_100, mset
```

Fixed parameters (env-overridable):

| Variable          | Default   | Meaning                                    |
|-------------------|-----------|--------------------------------------------|
| `BENCH_N`         | 100000    | Requests per command                       |
| `BENCH_CLIENTS`   | 50        | Concurrent clients                         |
| `BENCH_KEYSPACE`  | 100000    | Random keyspace (`-r`) to exercise memory  |
| `BENCH_PIPELINE`  | 10        | Pipeline depth for the pipelined suite     |
| `BENCH_TARGET_CPUS` | 0-3     | `cpuset-cpus` for the target container     |
| `BENCH_CLIENT_CPUS` | 4-7     | `cpuset-cpus` for the client container     |
| `BENCH_MEM_LIMIT` | 2g        | `--memory` on both target and client       |
| `BENCH_SUITE`     | (see above) | `-t` suite passed to valkey-benchmark    |
| `GOCACHE_IPC_IMAGE` | `gocache-bench:local-ipc` | IPC benchmark image tag |
| `IPC_PLUGINS` | `prometheus` | Space-separated IPC plugin list compiled into the IPC benchmark image |
| `BENCH_STATS` | `0` | Truthy values (`1`, `true`, `yes`, `on`) set `GOCACHE_BENCH_STATS=true` for in-process benchmark counters |
| `BENCH_IPC_EVENT_MODE` | `full` | IPC event attribution mode for `run-ipc.sh`: `full`, `events-off`, or `bridge-off`; Prometheus command metrics use `server:query:metrics.commands`, while the event switch remains for attribution and other event consumers |
| `BENCH_PUBSUB_N` | `10000` | PUBLISH requests per Pub/Sub fanout scenario |
| `BENCH_PUBSUB_FANOUTS` | `0,1,10` | Comma-separated subscriber counts to verify |
| `BENCH_PUBSUB_MESSAGE_BYTES` | `32` | Message payload size for Pub/Sub tests |
| `GOCACHE_PUBSUB_IMAGE` | `gocache-bench:local-pubsub` | Pub/Sub IPC benchmark image tag |
| `BENCH_PYTHON_IMAGE` | `python:3.12-alpine` | Client image used by the Pub/Sub fanout harness |

Total runtime ≈ 1 min per target invocation on modern hardware; the full matrix runs three target invocations. Pub/Sub runtime depends on `BENCH_PUBSUB_N` × fanout count.

## Fairness notes for the thesis

- Target and client run in separate containers on a shared bridge network, with fixed `cpuset-cpus` so neither noise nor resource contention from the host can skew one side vs the other.
- `valkey-server` starts with `--save "" --appendonly no` — persistence fully off, same as gocache's `--load-on-startup=false`. This excludes fsync variance.
- The client container is a fresh `docker run --rm` on each invocation, so no state leaks between runs.
- IPC runs use `failure_policy: halt_server` and wait for `prometheus`'s `/readyz` endpoint before measuring, so the run fails instead of silently measuring core-only traffic if the plugin does not register. They also capture `/telemetry` snapshots before the standard suite, after the standard suite, and after the pipelined suite; these files are diagnostic artifacts, not standalone performance results.
- `valkey-benchmark` is a self-paced closed-loop workload; it does not model a real open-loop latency distribution. Numbers are throughput-ceiling, not p99-under-target-tps. Useful for relative comparison, weaker for absolute latency claims. Document this caveat in the thesis.

## Known caveats

- Warm-up is implicit (valkey-benchmark itself warms up during the first few hundred commands). The first sample of each run is noisier than later samples.
- Docker bridge networking adds ~10–20 μs of per-RTT overhead vs unix-socket or host-networking. Both targets pay the same tax, so relative comparisons are unaffected.
- `lrange_100` pushes 100 elements into a list before measuring — the push phase is included in the `set` / `lpush` numbers depending on how valkey-benchmark's internal setup runs. Document this when quoting ops/sec for list reads.

## Rebuilding the gocache image

```bash
REBUILD=1 ./bench/redis-benchmark/run.sh <label> --target gocache
```

Or bump the image tag explicitly:

```bash
GOCACHE_IMAGE=gocache-bench:gc-opaque-index ./bench/redis-benchmark/run.sh gc-opaque-index --target gocache
```

This is the recommended pattern when moving between memory-optimization
labels so the image tag records which capture each cached image
corresponds to.
