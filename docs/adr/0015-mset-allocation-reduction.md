---
title: ADR-0015 MSET allocation reduction
description: Eliminate per-call map and slice allocations in MSET's hot path at 256 shards
status: accepted
date: 2026-05-19
deciders: [witherxse]
related:
  - Performance
  - ADR-0011-default-256-shards
  - ADR-0014-pipelined-performance-limits
---

# ADR-0015: MSET allocation reduction

## Context

After the shard count increase to 256 (ADR-0011), MSET's pipelined throughput gap vs Valkey is +64%. MSET already uses `TouchedShards` + `DispatchToShards` to lock only the touched subset (~10 shards for 10 keys on 256 shards), so the gap is not from bulk-locking all shards.

Profiling the hot path reveals three per-call allocations:

1. **`TouchedShards` map fallback**: `cache.go:525` allocates `make(map[int]struct{}, len(keys))` when `ShardCount > 64` because the uint64 bitset only covers 64 shards. With 256 shards this is every MSET call. Map allocation + GC pressure under P=10 pipelining (500 map allocations per pipeline burst across 50 connections).

2. **`perShard` slice**: `basic.go:239` allocates `make([][]cache.BulkPair, 256)` — 256 slice headers (4096 bytes) per MSET call. Most entries stay nil. Under pipelining, 500 allocations per burst.

3. **`keys` slice**: `basic.go:240` allocates `make([]string, 0, pairCount)` to collect keys for `TouchedShards`. This duplicates information already being computed during the shard-grouping loop.

Combined, each MSET call makes 3 heap allocations (~5KB) that are immediately discarded. Under pipelining this creates sustained GC pressure.

## Decision

### Fix 1: Extend `TouchedShards` bitset to 256 shards

Replace the single `uint64` with a `[4]uint64` array (256 bits). This covers shard counts up to 256 with zero allocation — the array lives on the stack. Use `math/bits.OnesCount64` instead of the manual popcount. The `map[int]struct{}` fallback remains for shard counts > 256 (not used in practice).

### Fix 2: Compute `TouchedShards` inline during shard grouping

`HandleMset` already iterates all keys to group by shard. Compute the touched-shard bitset during the same loop instead of calling `TouchedShards` separately. Eliminates the `keys` slice allocation and the second pass over keys.

### Fix 3: Pool the `perShard` slice

Use a `sync.Pool` for `[][]cache.BulkPair` slices. After use, nil out all entries and return to pool. Eliminates the 4096-byte allocation per call.

## Alternatives Considered

### Alternative 1: Reduce shard count for MSET path

- **Pros**: Fewer slice entries; smaller `perShard` allocation
- **Cons**: Shard count is a global config; can't differ per command. Lower shard count re-introduces contention for single-key commands.
- **Why not**: The fix should target MSET's allocations, not regress single-key performance.

### Alternative 2: Include MSET in batch coalescing

- **Pros**: Multiple pipelined MSETs could share one lock-acquisition umbrella
- **Cons**: Multi-key commands have variable touched-shard sets that must be merged across commands. Lock pre-acquisition becomes O(batch * keys-per-mset). The batch coalescing path assumes single-shard-per-command grouping; MSET violates that invariant. High complexity for marginal gain — the lock cost is structural (ADR-0014), not the allocation waste.
- **Why not**: Allocation reduction is simpler, lower risk, and addresses the non-structural portion of the gap.

## Consequences

### Positive

- `TouchedShards` goes from map allocation to zero allocation for N <= 256
- `HandleMset` eliminates one full pass over keys and the `keys` slice allocation
- `perShard` slice pooled — amortized zero allocation under sustained pipelining
- Reduced GC pressure under pipelined MSET workloads

### Negative

- `sync.Pool` for `perShard` adds a pool management code path. Mitigated: pool is local to `HandleMset`, no cross-goroutine coordination.

### Risks

- `[4]uint64` bitset is correct only for shard counts that are exact powers of 2 up to 256. Mitigation: `DefaultCacheShards` is always power-of-2 (enforced by bitmask selection in `shardIndex`). The map fallback handles arbitrary counts > 256.
