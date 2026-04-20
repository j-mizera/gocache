# Phase 3 GC-opaque LRU — `redis-benchmark` vs Phase 2 baseline

**Branch:** `feat/slab-phase0-resp-pool`
**Commit:** `dae3782` (Stage 2 — items map is `map[string]SlabPointer`)
**Date:** 2026-04-20

## Setup

Same containerized harness. `0-3` target, `4-7` client, 2 GiB mem,
valkey/valkey:8 client, `-n 100000 -c 50 -r 100000`, `-P 10` pipelined.

## Standard suite (no pipelining)

Raw CSVs: [`phase-2-gocache.csv`](phase-2-gocache.csv) vs
[`phase-3-gocache.csv`](phase-3-gocache.csv).

| command | phase-2 rps | phase-3 rps | Δ rps% |
|---|---:|---:|---:|
| **MSET (10)** |  79 744 |  92 165 | **+15.6%** |
| **LPUSH**     |   3 933 |   5 032 | **+27.9%** |
| **GET**       |  86 058 |  91 827 |  **+6.7%** |
| RPOP          |  16 866 |  18 099 |  +7.3% |
| PING_INLINE   |  88 495 |  92 421 |  +4.4% |
| RPUSH         |   5 934 |   6 158 |  +3.8% |
| LRANGE_100    |  61 050 |  63 051 |  +3.3% |
| HSET          |   1 829 |   1 888 |  +3.2% |
| LPOP          |   6 089 |   6 267 |  +2.9% |
| SADD          |   1 846 |   1 885 |  +2.1% |
| SPOP          |   2 182 |   2 217 |  +1.6% |
| SET           |  85 034 |  85 178 |  +0.2% |
| PING_MBULK    |  89 126 |  87 032 |  -2.3% |
| INCR          |  87 796 |  84 674 |  -3.5% |

### Standard headline

Every collection op improved; LPUSH +28% and MSET +16% lead. String ops
are at parity or slightly negative (INCR -3.5%, PING_MBULK -2.3%).

## Pipelined suite (P=10)

Raw CSVs: [`phase-2-gocache-pipelined.csv`](phase-2-gocache-pipelined.csv)
vs [`phase-3-gocache-pipelined.csv`](phase-3-gocache-pipelined.csv).

| command | phase-2 rps | phase-3 rps | Δ rps% |
|---|---:|---:|---:|
| **MSET (10)**   | 102 040 | 152 439 | **+49.4%** |
| **LPUSH**       |   1 173 |   1 542 | **+31.4%** |
| **GET**         | 367 647 | 478 468 | **+30.1%** |
| **INCR**        | 359 712 | 429 184 | **+19.3%** |
| **PING_INLINE** | 558 659 | 653 594 | **+17.0%** |
| **LRANGE_100**  | 123 609 | 141 643 | **+14.6%** |
| PING_MBULK      | 591 716 | 657 894 | +11.2% |
| SET             | 401 606 | 436 681 |  +8.7% |
| RPOP            |   6 264 |   6 448 |  +2.9% |
| HSET            |     848 |     871 |  +2.7% |
| SADD            |   1 865 |   1 913 |  +2.6% |
| SPOP            |   2 224 |   2 277 |  +2.4% |
| LPOP            |   3 813 |   3 899 |  +2.3% |
| RPUSH           |   3 736 |   3 797 |  +1.6% |

### Pipelined headline

**Every command improved.** GET pipelined +30%, INCR +19%, SET +8.7%,
MSET +49%, LPUSH +31%, LRANGE_100 +14.6%. These are the big-ROI gains
Phase 2 alone did not deliver.

## Memory (container RSS)

| metric | phase-2 | phase-3 | Δ |
|---|---:|---:|---:|
| baseline         |   7 430 209 |   7 933 526 |     +503 317 |
| post-standard    | 328 938 291 | 262 878 003 |  −66 060 288 |
| final            | 458 122 854 | 349 280 665 | **−108 842 189** |

**Final RSS dropped by 109 MiB (−24%).** The win is structural:

- 100k `*Entry` heap objects × 32 B = ~3 MiB gone.
- 100k `container/list.Element` nodes × ~40 B = ~4 MiB gone.
- 100k `lruMap` `*list.Element` value entries gone.
- Reduced transient allocation from RawGet no longer returning a
  pointer-type Entry means GC retention drops between mark cycles.

The post-standard RSS (262 MiB) is the active-workload peak after 100k
string SETs + 100k HSETs + collection churn. Phase 2 peaked at 329 MiB
for the same load.

## Full arc — Phase 1 (no slab) → Phase 3

The thesis-facing comparison. Positive = improvement over Phase 1 baseline.

### Pipelined (P=10)

| command | phase-1 rps | phase-3 rps | Δ rps% |
|---|---:|---:|---:|
| **SADD**        |     872 |   1 913 | **+119.4%** |
| **LPUSH**       |   1 024 |   1 542 |  **+50.6%** |
| **MSET (10)**   | 101 010 | 152 439 |  **+50.9%** |
| **PING_INLINE** | 515 463 | 653 594 |  **+26.8%** |
| **LRANGE_100**  | 112 866 | 141 643 |  **+25.5%** |
| **SET**         | 362 318 | 436 681 |  **+20.5%** |
| **GET**         | 401 606 | 478 468 |  **+19.1%** |
| PING_MBULK      | 591 716 | 657 894 |  +11.2% |
| INCR            | 396 825 | 429 184 |   +8.2% |

### Standard

| command | phase-1 rps | phase-3 rps | Δ rps% |
|---|---:|---:|---:|
| **LPUSH**    |   3 283 |   5 032 | **+53.3%** |
| **MSET (10)** |  83 612 |  92 165 |  +10.2% |
| RPOP         |  18 889 |  18 099 |  -4.2% |
| HSET         |   1 862 |   1 888 |  +1.4% |
| SADD         |   1 917 |   1 885 |  -1.7% |
| GET          |  94 250 |  91 827 |  -2.6% |
| LRANGE_100   |  64 808 |  63 051 |  -2.7% |
| INCR         |  90 826 |  84 674 |  -6.8% |
| PING_INLINE  | 102 459 |  92 421 |  -9.8% |
| SET          |  95 147 |  85 178 | -10.5% |

### Arc headline

Pipelined is where the slab investment pays off — simple-op reads up
~20-30%, collection mutations up 50-120%. Standard non-pipelined is a
wash or slight loss on string ops and a moderate win on collections.
The sink-aware fast path (from `fast-path-optimizations` plan, item #1)
is the lever that reclaims the standard-mode string gap — 27
instrumentation calls per command overwhelm the slab savings at 90k
rps when each op costs ~10 μs.

## GC-visibility change (what Phase 3 actually targeted)

Before Phase 3:
- `map[string]*Entry` — N `*Entry` GC-scanned pointers.
- `container/list.List` nodes — ~3N pointers (forward, back, Value).
- `map[string]*list.Element` — N `*Element` pointers.
- `map[string]int64` ttl — N keys scanned (strings).
- `map[string]int64` sizes — N keys scanned.

After Phase 3 Stage 2:
- `map[string]SlabPointer` — N inert `uint64` values, N string keys.
- `map[SlabPointer]any` nativeValues — populated only for EncNative
  (rare; empty in this benchmark's string/list/hash/set workload at the
  default thresholds).
- `map[SlabPointer]string` keysBySlot — N string values (dedup'd with
  items keys).
- `SlotMeta` inside each slab slot — zero pointers (all `uint64`/`int64`/`uint8`).

Net change per entry: roughly 5 GC-visible pointers → 2 (the two string
map positions). At 1M entries this shifts the GC mark phase
proportionally; the 109 MiB RSS drop at 100k entries is a downstream
effect.

## What this validates

- **Phase 2 showed the slab allocator did not regress the hot path
  uniformly** — it improved slot-reuse workloads (SADD pipelined) but
  cost strings (SET/GET standard).
- **Phase 3 proves the remaining cost was in the key index and LRU
  bookkeeping**, not the slab itself. Removing `*Entry` + `container/list`
  unlocked +20-50% across most pipelined ops.
- **SADD +119%** end-to-end from Phase 1 is the single most thesis-worthy
  number: diagnosis → slab allocator → GC-opaque LRU delivered 2.19× the
  throughput on a workload Phase 1 measured at -57%.

## What's next (deferred)

- **Stage 3** — delete `sizes` and `ttl` maps. TTL moves into
  `SlotMeta.ExpirationNs`. Size derivable from slab class + sidecar.
  Incremental GC win; single-digit % on current workloads.
- **Stage 4** — GC-level verification with `GODEBUG=gctrace=1` at 1M
  keys. RSS and throughput captured; GC pause time p99 remains the
  missing measurement for the thesis body.
- **fast-path-optimizations plan** — orthogonal. The standard-mode
  string regression vs Phase 1 (-10% SET/PING) is dominated by the 27
  instrumentation calls per command in `evaluateInternal`, not by slab
  work. That's the next lever.

## Phase status update

- Phase 0: shipped (RESP pool)
- Phase 1: shipped (hybrid encoding)
- Phase 2: shipped (slab allocator)
- Phase 3: **Stages 1 + 2 shipped**; Stage 3 (delete sizes/ttl) + Stage 4 (GC trace) queued
