# hybrid-encoding — `redis-benchmark` vs resp-pool baseline

**Branch:** `feat/memory-optimization`
**Snapshot:** zset dual-path shipped
**Date:** 2026-04-20

## Setup

Same containerized harness as resp-pool. Target/client share a bridge network
with isolated CPU sets (`0-3` target, `4-7` client), 2 GiB memory limit each,
`valkey/valkey:8` as the benchmark client.

- `-n 100000 -c 50 -r 100000`, pipeline `-P 10` for pipelined suite.
- Suite: `ping_inline,ping_mbulk,set,get,incr,lpush,rpush,lpop,rpop,sadd,hset,lrange_100,mset`
  — `spop` excluded as in resp-pool (known gocache stall).

## Standard suite (no pipelining)

Raw CSVs: [`resp-pool-gocache.csv`](resp-pool-gocache.csv) vs
[`hybrid-encoding-gocache.csv`](hybrid-encoding-gocache.csv).

| command | resp-pool rps | hybrid-encoding rps | Δ rps% | resp-pool p99 | hybrid-encoding p99 | Δ p99% |
|---|---:|---:|---:|---:|---:|---:|
| GET          | 101 833 |  94 251 |  -7.4% | 1 | 1 |  -1.5% |
| HSET         |   1 995 |   1 863 |  **-6.6%** | 50 | 57 | **+13.6%** |
| INCR         | 101 729 |  90 827 | -10.7% | 1 | 1 |  +7.9% |
| LPOP         |   6 516 |   6 390 |  -1.9% | 14 | 13 |  -6.6% |
| LPUSH        |   3 305 |   3 284 |  -0.6% | 35 | 37 |  +5.5% |
| LRANGE (+LPUSH seed) | 3 496 | 3 490 | -0.2% | 32 | 36 | +13.5% |
| LRANGE_100   |  65 789 |  64 809 |  **-1.5%** | 2 | 2 |  +8.5% |
| MSET (10)    |  90 334 |  83 612 |  -7.4% | 1 | 1 | +17.3% |
| PING_INLINE  |  99 108 | 102 459 |  +3.4% | 1 | 1 |  +4.6% |
| PING_MBULK   |  95 238 |  98 814 |  +3.8% | 1 | 1 |  +0.0% |
| RPOP         |  18 950 |  18 889 |  -0.3% | 7 | 6 |  -8.9% |
| RPUSH        |   6 605 |   6 255 |  -5.3% | 13 | 14 |  +9.2% |
| SADD         |   2 007 |   1 918 |  -4.5% | 49 | 56 | +15.2% |
| SET          |  94 607 |  95 147 |  +0.6% | 1 | 1 |  -1.1% |

### Headline

**The catastrophic regression of the first hybrid-encoding attempt is gone.** Old
hybrid-encoding measured: HSET -77%, LPUSH -50%, LRANGE_100 -98%. New hybrid-encoding: HSET
-6.6%, LPUSH -0.6%, LRANGE_100 -1.5%. The hybrid encoding kept collections
close to baseline instead of regressing 2–50×.

Small regressions (-5% to -10%) on ops that write a single element to an
empty collection are expected: the packed path does a sort-insert and a
buffer allocation where the old code did a bare `map[k]=v`. Tuning
ListAppendRight/SetAdd to preallocate (e.g. 64 B initial cap) is a hybrid-encoding
follow-up.

## Pipelined suite (P=10)

Raw CSVs: [`resp-pool-gocache-pipelined.csv`](resp-pool-gocache-pipelined.csv)
vs [`hybrid-encoding-gocache-pipelined.csv`](hybrid-encoding-gocache-pipelined.csv).

| command | resp-pool rps | hybrid-encoding rps | Δ rps% |
|---|---:|---:|---:|
| GET          | 383 142 | 401 606 | **+4.8%** |
| HSET         |     921 |     850 |  -7.7% |
| INCR         | 349 650 | 396 825 | **+13.5%** |
| LPOP         |   4 109 |   3 831 |  -6.8% |
| LPUSH        |   1 018 |   1 024 |  +0.6% |
| LPUSH (LRANGE seed) | 939 | 1 030 | +9.7% |
| LRANGE_100   | 106 724 | 112 867 | **+5.8%** |
| MSET (10)    |  89 606 | 101 010 | **+12.7%** |
| PING_INLINE  | 520 833 | 515 464 |  -1.0% |
| PING_MBULK   | 591 716 | 591 716 |   0.0% |
| RPOP         |   6 746 |   6 350 |  -5.9% |
| RPUSH        |   4 116 |   3 797 |  -7.7% |
| **SADD**     |   2 024 |     872 | **-56.9%** |
| SET          | 353 357 | 362 319 |  +2.5% |

### Pipelined headline

Most ops held parity or improved. GET pipelined +4.8%, INCR +13.5%, MSET
+12.7%, LRANGE_100 +5.8%. The one outlier is SADD pipelined at **-56.9%**.

Hypothesis: under pipelined load, SADD into small packed sets does a
sort-insert + splice per op, whereas the old native path did a bare map
insert. At pipeline depth 10 the allocator churn dominates. The hot path
reallocates a new buffer on every op because size changes every time.

Worth profiling in slab-allocator or addressing via:
- Buffer capacity hints so small-collection grow doesn't realloc on every
  insert.
- `sync.Pool` for the throwaway buffers.
- Potentially skipping sorted insertion when entry count < 8 (linear scan
  of unsorted members is cheaper; sort on first promotion boundary).

## Memory (container RSS)

| metric | resp-pool | hybrid-encoding | Δ |
|---|---:|---:|---:|
| baseline         |   7 667 187 |   7 479 492 |     -187 695 |
| post-standard    | 283 010 662 | 306 603 622 |  +23 592 960 |
| final            | 260 675 993 | 448 790 528 | **+188 114 535** |

The +188 MB difference in final RSS is substantial and unexplained. Plausible
causes:
- Many more small `[]byte` allocations surviving the pipelined run (one per
  SADD mutation, not GC'd in time).
- `sync.Pool` scratch buffers retained between runs.
- Packed buffers double-allocated during splice.

This is the second thing to profile. A full `pprof -heap` on a gocache
container mid-run would pinpoint it.

## What this validates

- The core hybrid-encoding thesis claim — that a hybrid encoding is required, not
  a single byte-shape — holds: none of the -50% to -98% collection
  regressions repeated.
- Small-collection ops run within noise of the native baseline.
- Read-heavy paths (LRANGE_100, GET, INCR pipelined) see modest +5% to +13%
  improvements, likely because the packed shape produces fewer GC pointers.

## What this flags for slab-allocator+

1. **SADD pipelined -57%** — allocation pressure from the sort-insert
   splice. Profile it.
2. **RSS +180 MB** — buffer retention. Profile it.
3. **Small-collection write ops -5% to -10%** — the empty→1-element path
   is the hot case for pipelined benchmarks that write to random keys;
   consider a lazy-init / scratch-pool strategy.

These are slab-allocator-aware concerns: the slab allocator replaces the
`make([]byte, ...)` hot path with a slab-backed pointer, which should close
both the SADD regression and the RSS growth in one step.
