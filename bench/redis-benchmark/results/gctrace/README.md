# gc-opaque-index GC trace

Harness: `bench/gctrace/main.go`. Fills N keys via `cache.RawSet`, forces
GC, reports `runtime.MemStats` before/after. GODEBUG=gctrace=1 output
captured in the `*.err` companion files.

**Date:** 2026-04-20
**gc-opaque-index snapshot:** sizes/ttl maps deleted, TTL in SlotMeta
**slab-allocator baseline:** `d16de60` (pre-gc-opaque-index)

## Strings @ 1M keys, 64 B value

| metric                | slab-allocator  | gc-opaque-index  | Δ           |
|-----------------------|---------:|---------:|------------:|
| HeapAlloc (post-load) | 367 MiB  | 231 MiB  | **−37%**    |
| HeapSys               | 451 MiB  | 291 MiB  | **−36%**    |
| HeapInuse             | 401 MiB  | 252 MiB  | −37%        |
| HeapObjects           | 4.01 M   | 1.01 M   | **−75%**    |
| Mallocs               | 6.02 M   | 3.02 M   | −50%        |
| NumGC (during load)   | 11       | 10       | −9%         |
| **gc1 after load**    | **28.9 ms** | **13.5 ms** | **−53%** |
| **gc2 after hold**    | **27.7 ms** | **12.0 ms** | **−57%** |
| Load throughput       | 872 k/s  | 1307 k/s | **+50%**    |

The HeapObjects delta (−3 M) is structural: slab-allocator held one
`*Entry` + one `container/list.Element` + two parallel map entries
(`sizes`, `ttl`) per key. gc-opaque-index replaces the first three with a
`SlabPointer` (value type, zero heap objects) and deletes the last two
in this stage. Net: ~3 GC-visible heap objects per key → 0.

The mark-phase drop (53–57% shorter GC) is the thesis-relevant
measurement: for a cache where keys significantly outnumber concurrent
operations, per-key GC scanning is the dominant pause cost.

## Hashes @ 500 k keys (3-field native maps)

| metric              | slab-allocator  | gc-opaque-index  | Δ        |
|---------------------|---------:|---------:|---------:|
| HeapAlloc           | 318 MiB  | 310 MiB  | −3%      |
| HeapObjects         | 3.51 M   | 2.01 M   | **−43%** |
| gc1 after load      | 26.4 ms  | 20.2 ms  | **−24%** |
| gc2 after hold      | 32.5 ms  | 19.7 ms  | **−39%** |
| Load throughput     | 698 k/s  | 809 k/s  | +16%     |

Smaller HeapAlloc delta because hashes are stored in `nativeValues` as
`map[string]string` — gc-opaque-index does not touch the native-map pointer
count, only the per-entry index/meta overhead. The −1.5 M heap-objects
delta matches the slab-allocator per-key overhead we removed (`*Entry` +
`list.Element` + `sizes` + `ttl` ≈ 3 objects × 500 k = 1.5 M).

GC mark-phase still drops 24–39% because the scavenger still needs to
walk fewer pointer-bearing roots overall — the native-map pointer
density is unchanged, but the index/LRU metadata that used to sit
beside each hash is gone.

## Pause-time distribution

Both phases keep p99 pause under 200 µs; the GC-opaque work is in the
*mark* phase (concurrent, throughput metric), not the stop-the-world
pause. This is expected — Go's GC pauses are bounded by the write
barrier flush, which this change does not affect.

| optimization    | NumGC | pause_mean_ns | pause_max_ns |
|-----------------|------:|--------------:|-------------:|
| slab-allocator  | 13    | 50 725        | 175 192      |
| gc-opaque-index | 12    | 48 880        | 173 220      |

No regression, no improvement — gc-opaque-index's win is concentrated in
concurrent mark work, not STW.

## Files

- `{slab-allocator,gc-opaque-index}-strings-1M.{out,err}` — 1M strings workload
- `{slab-allocator,gc-opaque-index}-hashes-500k.{out,err}` — 500k hashes workload
- `.out` — program summary (`runtime.MemStats` + slab stats)
- `.err` — `GODEBUG=gctrace=1` raw GC lines

## Reproduction

```bash
# Current HEAD:
GODEBUG=gctrace=1 go run ./bench/gctrace -keys=1000000 -mix=strings

# Compare against a baseline:
git worktree add /tmp/gocache-baseline <commit>
cp -r bench/gctrace /tmp/gocache-baseline/bench/gctrace
(cd /tmp/gocache-baseline && GODEBUG=gctrace=1 go run ./bench/gctrace -keys=1000000 -mix=strings)
```

## What this closes

- gc-opaque-index GC trace: **DONE**
- Thesis measurement — GC mark-phase duration before/after gc-opaque-index:
  **−53 to −57% on strings, −24 to −39% on hashes**

## What remains

No more slab-allocator work queued. Next lever is
`fast-path-optimizations` plan item #1 (sink-aware evaluator fast
path) — unrelated to GC, targets the −10% SET standard regression vs
hybrid-encoding that lives in the instrumentation layer.
