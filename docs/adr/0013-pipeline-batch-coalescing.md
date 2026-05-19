---
title: ADR-0013 Pipeline batch coalescing
description: Batch pipelined commands by shard to reduce lock acquisitions per pipeline burst
status: accepted
date: 2026-05-19
deciders: [witherxse]
related:
  - Performance
  - ADR-0010-direct-shard-mutex-dispatch
  - ADR-0011-default-256-shards
  - ADR-0012-read-lock-dispatch
---

# ADR-0013: Pipeline batch coalescing

## Context

When a client sends pipelined commands (N commands without waiting for replies), each command independently acquires and releases its target shard's lock. With 50 connections at pipeline depth 10, that is 500 individual lock acquisition/release cycles per pipeline burst.

The server connection loop already detects pipelined commands via `reader.Buffered() > 0` (used to batch response flushes), but does not batch the dispatch path. Each command follows: parse -> pipeline.Evaluate -> command.Dispatch -> engine.DispatchToShard -> Lock -> fn() -> Unlock -> return.

Dragonfly's `MultiCommandSquasher` (PR #1619) implements the same optimisation: when a connection's dispatch queue has multiple simple commands, it groups them by destination shard and executes each shard's batch as a single transaction hop. Their measurement: **10x improvement** on LPUSH pipeline benchmarks (12.2s to 0.9s for 1M commands).

The pattern is also used by Redis smart clients for cluster mode: go-redis groups pipelined commands by slot and dispatches each group in parallel.

## Decision

When the server connection loop detects buffered pipelined commands, it collects all available commands (up to 128), pre-computes each command's target shard, groups them by shard, acquires all needed shard locks in ascending order (deadlock-free), then evaluates all commands with a `ShardLocked` flag that causes `command.Dispatch` to skip lock acquisition (running the handler inline, like `InBatch` does for MULTI/EXEC).

Only single-key, non-transactional commands are batchable. Transactions, AUTH, QUIT, META, keyless, and multi-key commands terminate the batch and fall back to one-at-a-time processing.

When not pipelining (`reader.Buffered() == 0`), the code takes the existing single-command path with zero overhead.

### Lock mode per shard

For each shard in the batch, if ALL commands targeting that shard are `ReadOnly`, the shard is acquired with `RLock`; otherwise with `Lock`. This maximises read concurrency within batches.

### Correctness invariants

1. **Intra-shard ordering**: Commands on the same shard execute in pipeline submission order (the evaluate loop iterates in original order).
2. **Cross-shard independence**: Commands on different shards cannot have data dependencies (different keys, different shards).
3. **Deadlock prevention**: Shard locks acquired in ascending shard ID order, matching `cache.LockShards` used by `DispatchToShards`.
4. **Persistence ordering**: `wrapWithEmission` runs inside the handler closure under the shard lock; LSN allocation order matches lock order.

## Alternatives Considered

### Alternative 1: Per-shard channel + drain worker

- **Pros**: Eliminates lock contention entirely (single goroutine per shard); natural FIFO ordering
- **Cons**: Channel send/receive costs 100-250ns (4-25x more than mutex); adds goroutine management complexity; was the model replaced by ADR-0010
- **Why not**: ADR-0010 already demonstrated that direct mutex outperforms channels. Batch coalescing reduces the number of lock acquisitions (the remaining cost), achieving the same goal without re-introducing channels.

### Alternative 2: Flat combining

- **Pros**: One goroutine combines N operations under one lock; 2-4x improvement at 4-16 threads
- **Cons**: Requires thread-local publication slots (Go goroutines migrate between OS threads); worse than fine-grained locking on NUMA; complex to implement correctly in Go
- **Why not**: The Go-idiomatic equivalent (batch dispatch with pre-acquired locks) achieves the same lock amortisation without thread-local storage or CAS-elected combiners.

### Alternative 3: Adaptive batching with time window

- **Pros**: Collects more commands per batch by waiting a short time (e.g. 50us) for additional commands
- **Cons**: Adds latency to every pipelined command (at least 50us wait); complex tuning of window size vs throughput; harmful for non-pipelined latency-sensitive workloads
- **Why not**: The server already has natural batching from TCP buffering. When a client sends P=10, all 10 arrive in one TCP segment and are available in `reader.Buffered()`. An artificial wait adds latency without additional batching benefit.

## Consequences

### Positive

- Lock acquisitions per pipeline burst drop from N to the number of distinct shards touched (typically 1-10 for a 10-command pipeline on 256 shards)
- Zero overhead for non-pipelined workloads (gated by `reader.Buffered() > 0`)
- Compounds with ADR-0011 (more shards = fewer commands per shard in a batch = less time holding each lock) and ADR-0012 (read-only shard groups use RLock)

### Negative

- Holding multiple shard locks simultaneously while evaluating a batch. If a handler is slow (e.g. a large LRANGE), it delays release of all held locks. Mitigated: batch size is capped (128), and individual handler execution time is bounded by data structure complexity.
- Additional code paths in server connection loop and pipeline. The batch path is gated and falls back to single-command; both paths are tested.

### Risks

- A command handler that internally acquires a different shard's lock (re-entrant or cross-shard) would deadlock if that shard is already held by the batch. Mitigation: single-key commands only touch their assigned shard; multi-key commands are excluded from batching. The command routing matrix in `command.Dispatch` enforces this.
