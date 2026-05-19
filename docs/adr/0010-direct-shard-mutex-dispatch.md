---
title: ADR-0010 Replace channel dispatch with direct shard mutex
description: Eliminate per-shard engine goroutines and channel round-trips in favor of direct mutex locking
status: proposed
date: 2026-05-19
deciders: [witherxse]
related:
  - Server-Architecture
  - Performance
  - ADR-0009-rename-evaluator-to-pipeline
---

# ADR-0010: Replace channel dispatch with direct shard mutex

## Context

The engine (`pkg/engine`) serializes all single-key cache mutations through per-shard goroutines. Each command follows: channel send -> goroutine wake -> shard Lock() -> execute -> Unlock() -> channel send result -> caller wake. The `sendAndWait` function performs 2 channel operations and at least 2 goroutine context switches per command, costing ~800-1500ns overhead.

Benchmarks on `feat/collection-perf` show this is the dominant bottleneck for pipelined workloads. The actual cache operation takes ~1-5us, so dispatch overhead is 20-60% of total cost. Under pipelined pressure (16 commands in flight), all serialize through this channel one at a time.

Three of the four engine dispatch methods (`Dispatch`, `DispatchWithResult`, `DispatchToShards`) already bypass the goroutine and acquire shard locks directly. Only `DispatchToShard` -- the single-key hot path used by SET, GET, INCR, LPUSH, HSET, SADD, and every other per-key command -- goes through the channel.

Go channel internals: `runtime.chansend` acquires a mutex on the channel struct, copies the value, and potentially parks the goroutine. Wake-up may cross CPU cores (L1/L2 cache miss). A direct `sync.Mutex` under low contention (16-64 shards) takes the fast path (atomic CAS, ~10-20ns) almost always.

## Decision

We replace `DispatchToShard`'s channel-based path with direct shard mutex acquisition: `shard.Lock()` -> `fn()` -> `shard.Unlock()`. We remove the per-shard engine goroutines, the buffered command channels, and the result channel pool. The `Engine` struct becomes a stateless dispatch layer over `*cache.Cache` shard locks.

The `Engine.Run()` method is removed (no goroutines to start). `Engine.Stop()` retains the `atomic.Bool` stopped flag for shutdown coordination. All four dispatch methods now use the same direct-lock pattern.

## Alternatives Considered

### Alternative 1: Batch channel dispatch

- **Pros**: Amortizes channel cost across N commands in a pipeline batch; single channel send per batch
- **Cons**: Still pays channel overhead (divided by N); requires batching window that adds latency for non-pipelined single commands; more complex than direct mutex
- **Why not**: Direct mutex eliminates 100% of channel overhead with zero added latency, and is simpler to implement and reason about

### Alternative 2: Lock-free MPSC ring buffer

- **Pros**: ~5-10ns per operation via atomic CAS; no mutex contention
- **Cons**: Complex to implement correctly in Go (GC can move pointers mid-CAS); marginal improvement over direct mutex (~10-20ns) under low contention; debugging lock-free data structures is disproportionately expensive
- **Why not**: The improvement over direct mutex is ~10ns per command -- invisible in benchmarks given that cache operations themselves take 1-5us

### Alternative 3: Keep channel dispatch, add server-side batch reading

- **Pros**: No change to engine internals; server reads all available commands from TCP buffer before dispatching
- **Cons**: Commands still serialize one at a time through the channel within each batch; per-command overhead unchanged
- **Why not**: Batch reading optimizes flush timing (already implemented via `reader.Buffered() == 0` check) but does not reduce per-command dispatch cost

## Consequences

### Positive

- Per-command dispatch overhead drops from ~800-1500ns to ~10-20ns (40-75x reduction)
- Pipelined single-key operations (SET, GET, LPUSH, SADD, HSET) expected to improve 15-25%
- Engine code reduces from 228 lines to ~90 lines; `shardEngine`, `Command`, `resChanPool`, `sendAndWait` all deleted
- All four dispatch methods use the same pattern (direct lock), eliminating the conceptual split between "queued" and "direct" paths
- No goroutine leak risk from orphaned result channels on context cancellation

### Negative

- Goroutine pileup under extreme contention replaces channel backpressure as the saturation behavior; mitigated by shard count (16-64 shards distribute load)
- `Engine.Run()` removal changes the server startup sequence (one fewer goroutine launch)
- Engine tests that verify channel behavior must be rewritten

### Risks

- **Risk**: Deadlock if a handler called under `DispatchToShard`'s lock calls another dispatch method that tries to acquire the same shard. **Mitigation**: This is already prevented by the architecture -- handlers run their closure inside the lock and never call dispatch recursively. EXEC (which re-enters the pipeline) uses `DispatchWithResult` (bulk lock, not per-shard), and nested commands run inline (`inBatch=true`). The constraint is unchanged.
- **Risk**: Blocking operations (BLPOP/BRPOP) depend on engine goroutines. **Mitigation**: They do not. BLPOP uses `DispatchToShards` (already direct lock) for the non-blocking attempt and a separate waiter channel for blocking. The wake-up path uses `DispatchWithResult` (already direct lock).
