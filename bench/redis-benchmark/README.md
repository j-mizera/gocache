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
(no embedded observability — lean binary for fair comparison).

## Usage

```bash
# Baseline run on the current branch — capture both sides.
./bench/redis-benchmark/run.sh resp-pool --target gocache
./bench/redis-benchmark/run.sh resp-pool --target valkey

# After gc-opaque-index lands:
./bench/redis-benchmark/run.sh gc-opaque-index --target gocache
./bench/redis-benchmark/run.sh gc-opaque-index --target valkey

# Compare.
./bench/redis-benchmark/compare.sh resp-pool-gocache resp-pool-valkey
./bench/redis-benchmark/compare.sh resp-pool-gocache gc-opaque-index-gocache
./bench/redis-benchmark/compare.sh gc-opaque-index-gocache gc-opaque-index-valkey
```

Output per run lands under `bench/redis-benchmark/results/`:

- `<label>-<target>.csv` — standard suite (no pipelining)
- `<label>-<target>-pipelined.csv` — same suite with `-P 10`
- `<label>-<target>-memory.txt` — container RSS before/after + run metadata

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

Total runtime ≈ 1 min per invocation on modern hardware.

## Fairness notes for the thesis

- Target and client run in separate containers on a shared bridge network, with fixed `cpuset-cpus` so neither noise nor resource contention from the host can skew one side vs the other.
- `valkey-server` starts with `--save "" --appendonly no` — persistence fully off, same as gocache's `--load-on-startup=false`. This excludes fsync variance.
- The client container is a fresh `docker run --rm` on each invocation, so no state leaks between runs.
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
