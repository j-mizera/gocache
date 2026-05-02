# baseline-command-flow

Anchor for the `perf/command-flow-optimization` branch — captures the state of `main` (`1ea8c65`) right after `feat/memory-optimization` merged. Each child plan benchmark compares against this row.

**Date:** 2026-05-02
**Branch:** `perf/command-flow-optimization` (no code changes vs main)
**Commit:** `1ea8c65`
**Harness:** `bench/redis-benchmark/run.sh` — n=100 000, clients=50, keyspace=100 000, pipeline=10, target=cpus 0-3 / client=cpus 4-7, mem_limit=2 GiB
**Reference:** valkey 8 (`valkey/valkey:8`), same harness same limits

## Standard suite (no pipelining)

| Command | gocache rps | valkey rps | gocache / valkey |
|---|---:|---:|---:|
| PING_INLINE | 106 157 | 93 720 | **1.13×** |
| PING_MBULK | 104 821 | 92 936 | **1.13×** |
| SET | 100 300 | 95 510 | **1.05×** |
| GET | 101 010 | 91 827 | **1.10×** |
| INCR | 96 525 | 97 370 | 0.99× |
| MSET | 102 986 | 92 336 | **1.12×** |
| LRANGE_100 | 70 671 | 66 622 | **1.06×** |
| RPOP | 10 759 | 92 421 | 0.12× |
| LPUSH | 4 405 | 93 457 | **0.05×** |
| RPUSH | 3 611 | 90 744 | **0.04×** |
| LPOP | 3 628 | 96 993 | **0.04×** |
| HSET | 1 003 | 87 796 | **0.011×** |
| SADD | 1 016 | 91 743 | **0.011×** |
| SPOP | 1 194 | 89 365 | **0.013×** |

**Reading:** simple commands (PING/SET/GET/INCR/MSET) are at 99–113 % of valkey — the slab-allocator arc made this part of the system competitive. **Collection writes collapse 50–100×** below valkey at 50 concurrent clients with p50 latency ~50 ms, which is exactly the locking + per-command-instrumentation tax the four child plans target. LRANGE_100 stays competitive because the read handler dominates per-command overhead.

## Pipelined suite (P=10)

| Command | gocache rps | valkey rps | gocache / valkey |
|---|---:|---:|---:|
| PING_INLINE | 751 879 | 847 457 | 0.89× |
| PING_MBULK | 729 927 | 990 099 | 0.74× |
| SET | 490 196 | 781 249 | 0.63× |
| GET | 483 091 | 806 451 | 0.60× |
| INCR | 495 049 | 675 675 | 0.73× |
| MSET | 196 850 | 321 543 | 0.61× |
| LRANGE_100 | 152 905 | 197 628 | 0.77× |
| RPOP | 3 672 | 1 020 408 | 0.004× |
| LPUSH | 1 375 | 769 230 | 0.002× |
| RPUSH | 2 188 | 877 193 | 0.002× |
| LPOP | 2 209 | 884 955 | 0.002× |
| HSET | 469 | 917 431 | 0.001× |
| SADD | 1 042 | 990 099 | 0.001× |
| SPOP | 1 223 | 540 540 | 0.002× |

**Reading:** pipelined simple ops sit at 60–89 % of valkey — the engine-hop and per-command instrumentation amortize across the pipeline batch but don't disappear. **Pipelined collection writes are 0.1–0.4 %** of valkey: HSET at 469 rps with p50 = 1 063 ms is the worst point in the surface. Same root cause as standard mode but amplified by the channel hop being paid 10× per round trip.

## Memory

| | gocache | valkey | ratio |
|---|---:|---:|---:|
| baseline RSS | 7.6 MiB | 17.6 MiB | 0.43× |
| post-standard RSS | 225.3 MiB | 32.8 MiB | 6.86× |
| final RSS | 304.2 MiB | 36.6 MiB | 8.31× |
| **delta RSS** | **+296.6 MiB** | **+19.0 MiB** | **15.6×** |

The slab-allocator arc cut the cache-data portion of memory dramatically (gctrace at 1M strings: 230 MiB heap), but per-command tracking — operation registry + event replay ring (default 10 k events × ~500 B ≈ 5 MiB) + active-op map — adds steady-state overhead the harness picks up. Some of this should regress with the sink-aware fast path skipping registry insertion when no subscribers are attached.

## gctrace at 1M strings (64 B values)

From `bench/redis-benchmark/results/gctrace/baseline-command-flow-strings-1M.{out,err}`:

- HeapAlloc 230.65 MiB (post-load)
- HeapObjects 1.01 M
- gc1 after load: 12.08 ms
- gc2 after hold: 11.37 ms
- pause max: 0.18 ms
- load throughput: 1.10 M rps
- slab allocator: 1 000 001 live entries in class-0 (62 slabs, 62 MiB capacity, 61 MiB allocated)

These match the gc-opaque-index milestone — the slab arc held its gains across the merge to main.

## Where this baseline points the four child plans

| Plan | Targeted gap (vs valkey) | Expected post-plan |
|---|---|---|
| [sink-aware-fast-path](../../../../projects/gocache/plans/command-flow/sink-aware-fast-path) | std HSET 0.011×, std SADD 0.011×, std SPOP 0.013× | std collection writes ≥ 0.05–0.10× (5–10× rps) |
| [engine-pooling](../../../../projects/gocache/plans/command-flow/engine-pooling) | uniform 10–20 % allocation tax across all ops | flat improvement, ~10–20 % on every row |
| [read-lock-bypass](../../../../projects/gocache/plans/command-flow/read-lock-bypass) | pipelined GET 0.60×, HGET, LRANGE | pipelined reads → 0.95×+ valkey |
| [batched-pipelined-dispatch](../../../../projects/gocache/plans/command-flow/batched-pipelined-dispatch) | pipelined collection writes 0.001–0.004× | only revisit if first three leave headroom |

## Files

- `baseline-command-flow-gocache.csv` — standard suite gocache
- `baseline-command-flow-gocache-pipelined.csv` — pipelined suite gocache
- `baseline-command-flow-gocache-memory.txt` — RSS metadata gocache
- `baseline-command-flow-valkey.csv` — standard suite valkey
- `baseline-command-flow-valkey-pipelined.csv` — pipelined suite valkey
- `baseline-command-flow-valkey-memory.txt` — RSS metadata valkey
- `gctrace/baseline-command-flow-strings-1M.{out,err}` — gctrace 1M strings
- `gctrace/baseline-command-flow-hashes-500k.{out,err}` — gctrace 500k hashes
