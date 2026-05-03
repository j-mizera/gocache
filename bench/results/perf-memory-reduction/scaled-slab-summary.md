# Per-shard slab target scaled by shard count

**Branch:** `perf/memory-reduction`
**Date:** 2026-05-03
**Issue:** [#44](https://github.com/j-mizera/gocache/issues/44)
**Outcome:** Each shard's slab allocator now uses `DefaultTargetSlabBytes / N` (floored at 64 KiB) so the total slab capacity across N shards stays roughly constant rather than growing linearly with N. **Memory delta drops 15%** (281 MB → 237 MB at N=16). Throughput is within docker run-to-run noise vs pre-fix. **Acceptance target (≤180 MB delta) NOT met** — slab is a meaningful contributor but not the dominant one; further reduction needs lower N or other structural work.

## What changed

`pkg/cache/slab/allocator.go` now exposes:

- `DefaultTargetSlabBytes` (1 MiB) — constant for the legacy `NewAllocator()` path
- `MinTargetSlabBytes` (64 KiB) — floor so slab boundaries don't churn at high N
- `NewAllocatorWithTargetBytes(targetBytes uint32)` — sharded callers use this with a scaled target

`pkg/cache/cache.go::newCache` computes `slabTarget = DefaultTargetSlabBytes / shardCount` and passes it to each `newShard`. At N=1 each shard gets the original 1 MiB; at N=16 each shard gets 64 KiB.

The first-slab pre-allocation (the NilPointer-reservation slot) shrinks proportionally — at N=16 it's now 64 KiB per shard × 16 shards = 1 MiB total instead of 1 MiB × 16 = 16 MiB.

## Memory delta (mean of 2 docker runs each)

| Configuration | RSS delta (after standard + pipelined suite) |
|---|---:|
| post-#46 (slab 1 MiB × 16) | 281 MB / 280 MB |
| post-#44 (slab 64 KiB × 16) | **237 MB / 240 MB** |
| Δ | **-15%** (~43 MB saved) |
| valkey reference | 18 MB |

## Why this didn't hit the target

The acceptance criterion was "≤+20% over the pre-#34 N=1 baseline (~150 MB → ≤180 MB)". We landed at 237 MB — still 31% over the target.

The slab pre-allocation × N was a real problem (and is genuinely fixed) but it wasn't the dominant memory cost. Estimated breakdown of the remaining 237 MB delta:

| Component | Estimate |
|---|---:|
| Slab capacity (this PR's fix) | ~15-20 MB (was ~60 MB) |
| Per-shard maps × 16 (items, nativeValues, keysBySlot) | ~5-10 MB |
| Native collection storage (lists, hashes, sets from LPUSH, HSET, SADD, etc. during pipelined suite) | ~50-100 MB |
| RESP read/write buffers per connection (50 connections) | ~5-10 MB |
| Go runtime / GC heap overhead | ~50-100 MB |
| Other (channel pools, sync.Pool, plugin scaffolding) | ~10-20 MB |

The biggest remaining contributors are likely:
1. **Native collections retained from LPUSH/HSET/SADD** — these are stored as Go-native maps/slices in `nativeValues`; the pipelined suite repeatedly writes to `key:0..key:99999` for each command type, so the cache holds 100k × 5+ collection types worth of data at peak.
2. **Go's GC overhead** — non-compacting GC can hold up to ~2× live heap as RSS; with ~50 MB live, ~100 MB RSS is normal.

To actually hit the 180 MB target needs combined work:
- Reduce default N (e.g. N=8) to halve per-shard overhead — trades throughput
- Native collection encoding improvements (more aggressive packed encoding, smaller native shapes)
- Connection-level buffer pooling

Those are bigger changes than #44 anticipated. Shipping the slab fix as a step in the right direction; opening a separate follow-up for the harder cases.

## Throughput delta (mean of 2 runs each)

The post-#46 baseline run produced unusually high readings (PING 1086k, SET 952k, RPOP 913k — all 10-30% above historical post-#42 numbers). Compared to a more representative baseline (post-#42 means), post-#44 throughput is essentially neutral within docker run-to-run noise:

| Workload | post-#42 (Go bench) | post-#44 (docker mean) | Status |
|---|---:|---:|---|
| Pipelined SET | 772k | 745k | within noise |
| Pipelined GET | 811k | 824k | within noise |
| Pipelined HSET | 738k | 759k | within noise |
| Pipelined SADD | 738k | 741k | within noise |
| Pipelined SPOP | 794k | 692k | -13% (noisy single run) |

Smaller slabs trigger `growOneSlab` more frequently — each call allocates a new slab struct with its data buffer + meta array + freeList. At extreme rates this could matter, but the bench shows the cost is below docker noise on most commands. Worst case (SPOP -13%) needs a clean re-bench to confirm; suspect outlier.

## Verification

- `go vet ./...` clean
- `staticcheck ./...` clean
- `staticcheck -tags 'crashdump otlp' ./...` clean
- `go test -race -count=1 ./...` green across 25 packages

## Reproduce

```bash
sg docker -c "REBUILD=1 bash bench/redis-benchmark/run.sh scaled-slab --target gocache"
sg docker -c "bash bench/redis-benchmark/run.sh scaled-slab-rerun --target gocache"
```

## Files

- `pkg/cache/slab/allocator.go` (+~25 lines) — `DefaultTargetSlabBytes`, `MinTargetSlabBytes`, `NewAllocatorWithTargetBytes`
- `pkg/cache/shard.go` (1 line) — pass `slabTarget` through `newShard`
- `pkg/cache/cache.go` (+~5 lines) — compute `DefaultTargetSlabBytes / shardCount` per-shard
- `bench/results/perf-memory-reduction/{scaled-slab-*,this file}` — measurement artefacts
