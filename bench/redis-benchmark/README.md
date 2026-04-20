# redis-benchmark comparison harness

End-to-end throughput + latency benchmark for gocache using `redis-benchmark`
(or its BSD-licensed fork `valkey-benchmark`). Captures results in a stable
CSV format so Phases 0–3 of the slab-allocator refactor produce directly
comparable before/after numbers, and the thesis gets honest numbers to publish.

## Dependencies

One of these is required. The harness auto-detects whichever is on `$PATH`.

- **Arch / pacman**: `sudo pacman -S valkey` — ships `valkey-benchmark`, `valkey-server`, and friends. Redis is no longer in the extra repos as of 2024 because upstream Redis relicensed under SSPL; `valkey` is a community fork continuing under BSD.
- **Debian / Ubuntu**: `sudo apt install redis-tools` — ships `redis-benchmark` only (no server required).
- **macOS (brew)**: `brew install redis` or `brew install valkey`.

You can force a specific binary via `BENCH_CLIENT=/path/to/redis-benchmark ./run.sh …`.

## Usage

```bash
# From the repo root.
./bench/redis-benchmark/run.sh <label>
```

`<label>` becomes the filename prefix under `bench/redis-benchmark/results/`.
Convention: use the slab-allocator phase, e.g. `phase-0`, `phase-3`.

Output:

- `<label>.csv` — standard suite, no pipelining
- `<label>-pipelined.csv` — same suite with `-P 10`
- `<label>-memory.txt` — gocache process RSS before/after the run

## What the suite measures

Standard `redis-benchmark` command set:

```
ping_inline, ping_mbulk, set, get, incr, lpush, rpush, lpop, rpop,
sadd, hset, spop, lrange_100, mset
```

Fixed parameters:

- `-n 100000` requests per command
- `-c 50` concurrent clients
- `-r 100000` random key space (exercises more of the cache than default 1)
- Pipelined variant adds `-P 10`

These numbers give a reproducible signal within ~1 minute per run on modern hardware. Tune via env vars if you want longer / larger:

```bash
BENCH_N=500000 BENCH_CLIENTS=100 BENCH_KEYSPACE=1000000 ./run.sh phase-3-big
```

## Comparing phases

After capturing two runs:

```bash
./bench/redis-benchmark/compare.sh phase-0 phase-3
```

Prints a side-by-side table of `rps` and `p99_latency_ms` for each command,
plus the memory delta.

## Known caveats

- Warm-up is implicit (`redis-benchmark` itself warms up during the first hundred commands). The first sample of each run is therefore noisier than subsequent samples.
- `valkey-benchmark` and `redis-benchmark` produce byte-identical CSV for the same input; we treat them as interchangeable.
- `redis-benchmark` does not test TTL/expiry, pubsub, streams, or scripting — those aren't in the core scope and don't need to be in the comparison.
- The server starts with empty state and no persistence (`--load-on-startup=false`, snapshot path in a temp dir) so results reflect steady-state throughput, not cold-start cost.
