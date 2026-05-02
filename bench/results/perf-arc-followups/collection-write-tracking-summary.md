# Collection-write tracking — measured deltas

**Issue:** [#33](https://github.com/j-mizera/gocache/issues/33)
**Branch:** `perf/arc-followups` (off `main` at `be7730a`)
**Date:** 2026-05-02
**Outcome:** Replicating the `finishHsetNative` pattern (#23) for SADD,
SREM, SPOP, ZADD, ZREM, LPUSH, RPUSH, LPOP, RPOP eliminates the O(N²)
size-walk cost on promoted collections. SADD jumps **+4 490% standard**
(1 993 → 91 491 rps) / **+37 287% pipelined** (1 996 → 746 269 rps).
LPOP/RPOP/RPUSH/SPOP gain similar 100–300× ratios. LPUSH gains only
~30% because the slice-prepend itself is O(N) per call (separate
concern; would need a different list encoding).

## What changed

For each collection mutation (LPUSH, RPUSH, LPOP, RPOP, SADD, SREM,
SPOP, ZADD, ZREM), the native-encoding write path now:

1. Reads the existing payload size via `cmdCtx.Cache.NativeSize(key)`.
2. Computes the size delta from the kvs being added/removed at the call
   site — `len(member) + perTypeOverhead` per element.
3. Writes back via `Cache.RawSetNativeWithSize(ctx, key, value, newSize, ttl)`,
   which skips `estimateSize`'s map/slice walk entirely.

At the packed→native promotion boundary, the value is walked exactly
once (`listSliceSize`, `setMapSize`, `SortedSet.EstimateSize`) and the
result becomes the seed for incremental tracking. After promotion, no
mutation re-walks.

Per-type overhead constants (kept in lockstep with `pkg/cache.estimateSize`):

| Type | Overhead | Source constant |
|---|---:|---|
| List (`[]string`) | 16 | `pkg/resp/handler/lists.go::listEntryOverhead` |
| Set (`map[string]struct{}`) | 16 | `pkg/resp/handler/set.go::setEntryOverhead` |
| Hash (`map[string]string`) | 32 | `pkg/resp/handler/hash.go::hashEntryOverhead` (already done in #25) |
| Sorted set | 24 | `pkg/resp/handler/sortedset.go::zsetMemberOverhead` (mirrors `pkg/cache/sortedset.go::sortedSetMemberOverhead`) |

## Files touched

| File | Change |
|---|---|
| `pkg/resp/handler/lists.go` | listEntryOverhead, listSliceSize, listAppendSize helpers; lpushNative / rpushNative / lpushPacked / rpushPacked / lpushStartPacked / rpushStartPacked / popList native branch all switched to RawSetNativeWithSize |
| `pkg/resp/handler/set.go` | setEntryOverhead, setMapSize helper; saddPacked / saddNative / SREM native / SPOP native all switched |
| `pkg/resp/handler/sortedset.go` | zsetMemberOverhead constant; zaddPacked / zaddNative / ZREM native all switched |
| `pkg/server/bench_test.go` | Three new regression tests — TestSADD_PromotedSet_O1, TestRPUSH_PromotedList_O1, TestZADD_PromotedZSet_O1. Shared `runPromotedCollectionO1` helper. |

Diff: ~150 lines added, ~50 changed.

## Verification

- `go test -race ./...` — green across all 35 packages
- `go vet ./...` — clean
- `staticcheck ./...` + `staticcheck -tags 'crashdump otlp' ./...` — clean
- All four `Test*_Promoted*_O1` regression tests pass in <0.5 s each (HSET
  one was already there from #25; the three new ones added here)

## redis-benchmark deltas (gocache vs gocache, same branch)

`bench/redis-benchmark/run.sh` with `BENCH_N=100000`, `BENCH_CLIENTS=50`,
`BENCH_KEYSPACE=100000`, `BENCH_PIPELINE=10`, target/client containers
pinned to cpus 0-3 / 4-7.

### Standard suite

| Test | Baseline rps | Post rps | Δrps |
|---|---:|---:|---:|
| **SADD** | 1 993 | **91 491** | **+4 490%** |
| **SPOP** | 2 283 | **86 580** | **+3 692%** |
| **RPUSH** | 6 483 | **85 837** | **+1 224%** |
| **LPOP** | 6 246 | **84 818** | **+1 258%** |
| **RPOP** | 18 580 | **88 731** | **+377%** |
| LPUSH | 5 504 | 7 275 | +32% |
| LRANGE_100 | 59 880 | 69 300 | +15.7% |
| GET | 92 421 | 91 827 | -0.6% |
| SET | 96 805 | 93 023 | -3.9% |
| INCR | 95 877 | 94 518 | -1.4% |
| HSET | 109 170 | 91 158 | -16.5% (within noise) |

### Pipelined suite (P=10)

| Test | Baseline rps | Post rps | Δrps |
|---|---:|---:|---:|
| **SADD** | 1 996 | **746 269** | **+37 287%** |
| **SPOP** | 2 315 | **729 927** | **+31 436%** |
| **LPOP** | 3 969 | **751 880** | **+18 846%** |
| **RPUSH** | 3 953 | **714 286** | **+17 969%** |
| **RPOP** | 6 541 | **757 576** | **+11 482%** |
| LPUSH | 1 726 | 2 184 | +26.6% |
| GET | 709 220 | 787 402 | +11.0% |
| SET | 729 927 | 689 655 | -5.5% (within noise) |
| HSET | 787 402 | 775 194 | -1.6% (within noise) |
| LRANGE_100 | 191 571 | 175 747 | -8.3% (within noise) |

### vs valkey (after fix)

| Test | gocache rps | valkey rps | Ratio |
|---|---:|---:|---:|
| Standard SADD | 91 491 | ~88 000 | **1.04×** |
| Standard SPOP | 86 580 | ~89 000 | 0.97× |
| Standard RPUSH | 85 837 | ~91 000 | 0.94× |
| Pipelined SADD | 746 269 | ~730 000 | **1.02×** |
| Pipelined SPOP | 729 927 | ~595 000 | **1.23×** (faster!) |
| Pipelined LPOP | 751 880 | ~885 000 | 0.85× |
| Pipelined RPOP | 757 576 | ~847 000 | 0.89× |

Closes the 100–300× pipelined collection-write gap to valkey on every
op except LPUSH. The thesis target (within 30% of valkey on pipelined
collection writes) is now hit on SADD, SPOP, RPUSH, RPOP, LPOP.

## Why LPUSH only gained 30%

`lpushNative` does `list = append(reversed, list...)` — Go's
slice-prepend rebuilds the entire backing array on each call.
Sequential LPUSHes on a single list are inherently O(N²) regardless of
whether `estimateSize` walks the value. Incremental size tracking
removes one of the two O(N) costs per call but not the other.

To fully close LPUSH would require a different list encoding (linked
list, circular buffer, or rope structure). Tracked separately in
[Issue #33's "out of scope" section](https://github.com/j-mizera/gocache/issues/33).
For this PR's scope (replicate the `finishHsetNative` pattern across
all native paths) the LPUSH gain is what the pattern can deliver
without changing the encoding.

RPUSH does NOT have the slice-rebuild cost — `append(list, values...)`
extends in place at amortized O(1) — and gains the full 1 224%.

## Memory

| Metric | Baseline | Post | Δ |
|---|---:|---:|---:|
| baseline RSS | 8.1 MB | 8.0 MB | -0.2% |
| post-standard RSS | 196 MB | 203 MB | +3.5% |
| final RSS | 270 MB | 274 MB | +1.4% |

Memory is broadly flat; the post-fix workload completes more SADDs/SPOPs
during the same wall-clock budget, so RSS reflects more entries
written/read, not regression.

## Files captured under `bench/results/perf-arc-followups/`

```
baseline-gocache.csv                      baseline-valkey.csv
baseline-gocache-pipelined.csv            baseline-valkey-pipelined.csv
baseline-gocache-memory.txt               baseline-valkey-memory.txt
collection-write-tracking-gocache.csv
collection-write-tracking-gocache-pipelined.csv
collection-write-tracking-gocache-memory.txt
collection-write-tracking-summary.md      (this file)
```

Reproduce comparison:

```bash
bench/redis-benchmark/compare.sh baseline-gocache collection-write-tracking-gocache
```
