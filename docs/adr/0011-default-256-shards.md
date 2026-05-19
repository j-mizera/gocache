---
title: ADR-0011 Increase default shard count to 256
description: Reduce per-shard mutex contention by increasing cache shards from 8 to 256
status: accepted
date: 2026-05-19
deciders: [witherxse]
related:
  - Performance
  - ADR-0010-direct-shard-mutex-dispatch
---

# ADR-0011: Increase default shard count to 256

## Context

`DefaultCacheShards` is 8 (`api/config/config.go:27`). Under pipelined workloads with 50 concurrent connections (the `redis-benchmark` default), each shard receives ~6.25 concurrent goroutines contending on its `sync.RWMutex`.

Go's `sync.Mutex` enters **starvation mode** when any waiter exceeds 1ms, switching from throughput-optimised barging to strict FIFO handoff. On a 96-core machine (Go issue #33747), this causes performance to collapse from 38x baseline at 45 workers to 5x baseline at 51 workers. With 6+ goroutines per shard and collection operations holding locks for 10-50us, starvation mode is reachable under burst pipelining.

Post-dispatch-rework benchmarks (ADR-0010) show pipelined collection ops (LPUSH, HSET, SADD) regress -25% to -36% vs Valkey. The dominant factor is per-shard contention at 8 shards, not the direct-mutex mechanism itself.

Every production Go cache library uses 256-1024 shards: ristretto (256), bigcache (1024), freecache (256), imcache (256). The shard selection path (`fnv1a(key) & (n-1)`) already uses power-of-2 bitmask selection.

## Decision

Change `DefaultCacheShards` from 8 to 256. The constant lives in `api/config/config.go`; the cache constructor at `pkg/cache/cache.go:176` (`NewWithShards`) and shard routing at `cache.go:282` (`shardIndex`) require no changes since they already support arbitrary power-of-2 counts.

Users with an explicit `memory.cache_shards` config are unaffected.

## Alternatives Considered

### Alternative 1: 64 shards

- **Pros**: Lower base memory (~32KB vs ~128KB); still reduces contention 8x
- **Cons**: At 50 connections, still ~0.8 goroutines per shard -- marginal. Under burst pipelining (P=10), 500 in-flight ops / 64 shards = ~7.8 per shard, still in the starvation danger zone
- **Why not**: Does not solve the problem at realistic benchmark concurrency levels

### Alternative 2: 1024 shards

- **Pros**: Even lower per-shard contention (~0.05 goroutines/shard at 50 connections)
- **Cons**: ~512KB base memory; diminishing returns vs 256 (both are effectively zero contention); more shards to iterate for full-keyspace ops (KEYS, SCAN, FLUSHDB, snapshot)
- **Why not**: 256 already reduces contention to ~0.2 goroutines/shard, which is below the threshold where mutex contention matters

## Consequences

### Positive

- Per-shard contention drops from ~6.25 to ~0.2 goroutines at 50 connections (32x reduction)
- Starvation mode becomes unreachable under normal workloads
- Pipelined collection ops expected to improve significantly (primary bottleneck removed)

### Negative

- Base memory increases from ~4KB (8 shards) to ~128KB (256 shards) -- negligible
- Full-keyspace operations (FLUSHDB, snapshot, KEYS) iterate 256 shards instead of 8 -- lock acquisition loop is longer but per-shard work is proportionally less

### Risks

- Workloads with very few keys may spread them across more shards than necessary, reducing data locality. Mitigation: at low key counts, the hot entries stay in L1/L2 regardless of shard assignment.
