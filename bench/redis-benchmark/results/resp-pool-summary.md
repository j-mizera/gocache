# resp-pool baseline — gocache vs valkey

Captured 2026-04-20, commit `f0afdca` on `feat/memory-optimization`.

Both servers containerized, identical limits:

- Image: gocache-bench:local (built from repo root `Dockerfile` with `PLUGINS=""`) vs `valkey/valkey:8`
- `--cpuset-cpus 0-3` (target), `--cpuset-cpus 4-7` (client), `--memory 2g`
- Client: `valkey-benchmark` running in a separate container on the same bridge network
- n=100 000, c=50, r=100 000, pipeline=10 for the pipelined variant

## Standard suite — ops/sec

| Command      | gocache   | valkey    | gocache / valkey |
|--------------|----------:|----------:|-----------------:|
| PING_INLINE  |    99 108 |   100 705 | 98%              |
| PING_MBULK   |    95 238 |   100 503 | 95%              |
| SET          |    94 607 |   102 249 | 93%              |
| GET          |   101 833 |   105 820 | 96%              |
| INCR         |   101 729 |   105 042 | 97%              |
| MSET (10 k)  |    90 334 |   102 354 | 88%              |
| **LPUSH**    |     3 305 |   101 010 | **3.3%**         |
| **RPUSH**    |     6 605 |   103 413 | **6.4%**         |
| **LPOP**     |     6 516 |   103 093 | **6.3%**         |
| **RPOP**     |    18 950 |   103 199 | **18.4%**        |
| **SADD**     |     2 007 |   104 058 | **1.9%**         |
| **HSET**     |     1 995 |   106 724 | **1.9%**         |
| **SPOP**     |     2 392 |   108 342 | **2.2%**         |
| LRANGE_100   |    65 789 |    71 788 | 92%              |

## Pipelined suite (P=10) — ops/sec

| Command      | gocache   | valkey      | gocache / valkey |
|--------------|----------:|------------:|-----------------:|
| PING_INLINE  |   520 833 |     990 099 | 53%              |
| PING_MBULK   |   591 716 |   1 030 928 | 57%              |
| SET          |   353 357 |     813 008 | 43%              |
| GET          |   383 142 |     970 874 | 39%              |
| INCR         |   349 650 |     961 538 | 36%              |
| LRANGE_100   |   106 724 |     203 666 | 52%              |
| MSET (10 k)  |    89 606 |     300 300 | 30%              |
| **LPUSH**    |     1 018 |     714 286 | **0.14%**        |
| **RPUSH**    |     4 116 |   1 000 000 | **0.41%**        |
| **HSET**     |       921 |     806 452 | **0.11%**        |
| **SADD**     |     2 024 |     943 396 | **0.21%**        |
| **SPOP**     |         2 |     571 429 | **0.0003%**      |

## Memory (container RSS)

| Metric                   | gocache (bytes) | valkey (bytes) | gocache / valkey |
|--------------------------|----------------:|---------------:|-----------------:|
| Baseline (empty)         |       7 667 187 |     19 786 629 | 39%              |
| After standard suite     |     283 010 662 |     35 431 383 | 799% (**8.0x**)  |
| Final (after pipelined)  |     260 675 993 |     39 447 429 | 661% (**6.6x**)  |
| Delta (final − baseline) |     253 008 806 |     19 660 800 | **12.9x**        |

## Interpretation

**The simple-string commands are close to parity.** GET/SET/INCR/PING land at 93–97% of valkey standard, which is the expected overhead for a young implementation in Go versus a battle-hardened C server. The resp-pool pool refactor is partly why they're this close — the zero-alloc RESP write path matters most on short-response commands.

**Collection commands are a disaster.** LPUSH/HSET/SADD at 2–3 k rps (vs valkey's 100 k+) is a 30–50× gap, and it collapses further under pipelining. SPOP at **2 rps pipelined** versus 571 k for valkey — four and a half orders of magnitude — is pathological.

The cause is almost certainly the current per-mutation work on `pkg/cache`:

1. `estimateSize` uses reflection to walk each collection's contents on every mutation, updating the per-entry `sizes` map + cumulative `usedBytes`.
2. `Entry.Value any` holding a Go-native `[]string` / `map[string]string` / set means every append triggers reallocation + GC pressure.
3. The `lruMap` + `list.Element` + `sizes` + `store` map chain means **every mutation writes to at least four maps**.

hybrid-encoding (byte-oriented `Entry`) targets (1) and (2): flat `[]byte` payloads mean `len(value)` is the size (no walking), and one allocation per mutation instead of one-per-element. gc-opaque-index then folds `lruMap` and `sizes` into the slab header, cutting the map write count.

**Memory is the second headline.** gocache holds 12.9× the RSS growth of valkey for the same workload. Every `map[string]*Entry` + `*Entry.Value any` + `list.Element` + `time.Time` multiplies GC-tracked pointers. This is exactly the number the slab allocator (slab-allocator + 3) is built to bring down.

## Success criteria for gc-opaque-index

To keep the thesis claim defensible, after gc-opaque-index:

- Collection write commands (LPUSH/HSET/SADD/SPOP) should land at **≥30%** of valkey ops/sec standard and **≥20%** pipelined. Getting to parity is unlikely for a Go implementation; narrowing the gap from 50× to 3–5× is the achievable story.
- Memory delta should drop from 12.9× to **≤3×** valkey's RSS growth.
- Simple commands must not regress below the current 93% parity (they're already close).

Full baselines preserved under `bench/redis-benchmark/results/resp-pool-*`. After gc-opaque-index lands, re-run both `--target gocache` and `--target valkey` with the same label convention (`gc-opaque-index-*`) and cross-compare.
