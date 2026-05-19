---
title: ADR-0012 Read-lock dispatch for read-only commands
description: Use RLock instead of exclusive Lock for read-only command dispatch to allow concurrent readers
status: accepted
date: 2026-05-19
deciders: [witherxse]
related:
  - Performance
  - ADR-0010-direct-shard-mutex-dispatch
  - ADR-0011-default-256-shards
---

# ADR-0012: Read-lock dispatch for read-only commands

## Context

Every command dispatch through `Engine.DispatchToShard` acquires an exclusive write lock (`shard.Lock()`), even for read-only commands like GET, HGET, LRANGE, SCARD, and TTL. This means concurrent reads on the same shard serialize, blocking each other unnecessarily.

The infrastructure for read-only classification already exists:

- `command.Spec.ReadOnly` (field at `api/command/types.go:44`) is annotated on every command
- `readonly_test.go` pins the classification for all 72+ registered commands
- `wrapWithEmission` (`pkg/command/context.go:231`) already short-circuits for `ReadOnly`, skipping persistence emission

Each shard uses `sync.RWMutex` (`pkg/cache/shard.go:36`) with exported `RLock()`/`RUnlock()` methods. However, `Engine.DispatchToShard` always calls `s.Lock()` -- the `RLock` path is never used for command dispatch.

A previous attempt (issue #28, documented at `pipeline.go:218-227`) tried routing reads inline under a cache-wide `RLock()` to bypass the channel-based engine. That failed because `sync.RWMutex` mode-switching (alternating between `RLock` and `Lock` on the same mutex) incurred overhead that offset the read gains. However, that was under two conditions that no longer apply: (1) the engine used per-shard goroutines with channels (removed in ADR-0010), and (2) the cache had 8 shards (increased to 256 in ADR-0011). With 256 shards, the probability of rapid RLock/Lock mode-switching on any single shard is negligible.

Benchmark measurements show `sync.RWMutex.RLock` is ~3x faster than `Lock` under contention (45ns vs 142ns at 24 cores).

## Decision

Add `Engine.DispatchToShardRO(ctx, shard, fn)` that acquires `shard.RLock()`/`shard.RUnlock()` instead of the exclusive lock. Modify `command.Dispatch` to route through `DispatchToShardRO` when `ctx.Spec.ReadOnly` is true and the command targets a single shard (`ctx.Shard >= 0`).

## Alternatives Considered

### Alternative 1: xsync.RBMutex (reader-biased mutex)

- **Pros**: 22x improvement over `sync.RWMutex` at 64 cores by sharding the reader counter across CPU-local slots
- **Cons**: External dependency (`puzpuzpuz/xsync`); overkill at current deployment scale (typically 4-16 cores)
- **Why not**: `sync.RWMutex` is sufficient at 256 shards where per-shard contention is near zero. RBMutex becomes relevant at 64+ cores with high reader counts per shard.

### Alternative 2: RCU (read-copy-update) via atomic.Pointer

- **Pros**: Zero-cost reads (atomic pointer load, no lock)
- **Cons**: O(N) copy per write; only viable for small/infrequently-mutated data. A cache shard with thousands of keys under write load would be pathological.
- **Why not**: Cache shards are write-heavy (SET/LPUSH/HSET are core workload). RCU copy cost dominates.

## Consequences

### Positive

- Concurrent reads on the same shard proceed in parallel instead of serializing
- ~3x improvement for read ops under per-shard contention
- Typical cache workloads (80%+ reads) benefit disproportionately

### Negative

- RWMutex mode-switching overhead when reads and writes interleave on a single shard. Mitigated by 256 shards making per-shard interleaving rare.
- Slightly more complex dispatch routing in `command.Dispatch` (one additional branch)

### Risks

- A mutating command incorrectly classified as `ReadOnly` would execute under `RLock`, causing a data race. Mitigation: `readonly_test.go` pins every command's classification; the test fails if a new command is registered without an explicit ReadOnly assertion.
