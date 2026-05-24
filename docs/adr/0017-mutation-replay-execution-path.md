---
title: ADR-0017 Mutation replay execution path
description: ApplyMutation on CacheStore dispatches replayed mutations to Raw* methods, keeping op-knowledge on the cache side of the contract boundary
status: proposed
date: 2026-05-21
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0002-source-sink-contract
  - ADR-0016-aof-wire-and-file-format
---

# ADR-0017: Mutation replay execution path

## Context

The persistence coordinator has a `replayInto` function that iterates over mutations from a Source's `ReplayIterator` during boot. Currently this function advances the LSN cursor but does not re-execute the mutations against the cache — it's a stub waiting for a replay path.

AOF boot (ADR-0016) requires replaying every recorded mutation to reconstruct the pre-crash state. The question is: where does the op-dispatch logic live? Something has to map `Mutation{Op: "SET", Args: [key, value]}` to the actual cache write.

Three layers could own this:

1. **The cache** (`pkg/cache/store.go`) — it already has `Raw*` methods for every data type.
2. **The evaluator/pipeline** (`pkg/pipeline/`) — it has command handlers, but they also fire hooks, watch notifications, and persistence emission.
3. **The coordinator** (`pkg/persistence/coordinator.go`) — it orchestrates boot, but it's a persistence-layer component that shouldn't know about command semantics.

## Decision

Add `ApplyMutation(ctx context.Context, m Mutation) error` to the `CacheStore` interface (`api/persistence/store.go`). The implementation lives on `*Cache` in `pkg/cache/store.go` and dispatches on `m.Op` to existing `Raw*` methods.

The coordinator's `replayInto` function calls `store.ApplyMutation(ctx, m)` for each mutation yielded by the iterator. On error, it logs a warning and continues — resilient recovery over strict correctness, same pattern as `loadSnapshotInto` which skips malformed entries.

`ApplyMutation` must handle every mutation-emitting command: SET, DEL, HSET, HDEL, SADD, SREM, ZADD, ZREM, LPUSH, RPUSH, LPOP, RPOP, EXPIRE, PEXPIRE, INCR, DECR, INCRBY, DECRBY, INCRBYFLOAT, APPEND, MSET, RENAME, SETNX, SPOP, FLUSHDB, FLUSHALL, GETSET, GETDEL. Unknown ops return an error (logged and skipped).

Boot is single-threaded, so per-key shard locking via `c.Shard(key)` is sufficient — no special replay-mode locking needed.

`Mutation.Args` follows the same layout as command handlers receive: `Args[0]` = key, `Args[1+]` = value/options.

## Alternatives Considered

### Alternative 1: Re-dispatch through the evaluator/pipeline

- **Pros**: Reuses existing command handlers — every command is already implemented. No new code to maintain.
- **Cons**: Creates a circular dependency: `pkg/persistence` would import `pkg/pipeline`, but `pkg/pipeline` already depends on the persistence coordinator for mutation emission. During replay, re-dispatching through the pipeline would fire hooks, watch notifications, and — critically — re-emit mutations to sinks, creating an infinite replay loop. Disabling emission during replay requires threading a "replay mode" flag through the entire pipeline, which is fragile and invasive.
- **Why not**: The circular dependency is a hard blocker. Even without it, replaying through the pipeline re-fires side effects (hooks, watch) that are semantically wrong during boot.

### Alternative 2: Inline case-switch in the coordinator

- **Pros**: Self-contained — all replay logic in one place. No interface changes.
- **Cons**: Puts command-semantic knowledge (what "SET" means, how "HSET" works) into the persistence layer, which should be op-agnostic. The coordinator knows about Sources, Sinks, LSNs, and batches — not about data types. This conflates replay with snapshot loading, making the coordinator responsible for two different recovery mechanisms with different semantics.
- **Why not**: Wrong layer. The persistence coordinator is a transport/lifecycle component. Op-dispatch belongs where the data types are defined.

### Alternative 3: Separate replay engine

- **Pros**: Clean separation — a dedicated `pkg/replay/` package owns mutation→cache dispatch.
- **Cons**: Over-engineered. The dispatch is a single `switch` statement calling existing methods. A new package adds a directory, a new import path, and a new testing surface for what amounts to ~100 lines of glue code. The cache already exposes every method the replay needs.
- **Why not**: The simplest correct location is on `*Cache` itself, which already implements `CacheStore`. Adding a method to the existing interface is less machinery than a new package.

## Consequences

### Positive

- Replay is op-correct by construction — `ApplyMutation` calls the same `Raw*` methods that live command handlers use.
- No circular dependencies. The dependency graph stays: `api/persistence` ← `pkg/cache` (implements interface), `api/persistence` ← `pkg/persistence` (uses interface).
- No side effects during replay — hooks, watch, and mutation emission are all bypassed because `ApplyMutation` calls `Raw*` methods directly, not pipeline handlers.
- Unknown ops are logged and skipped, so a v2 AOF file with new ops can be partially replayed by a v1 binary without crashing.

### Negative

- `ApplyMutation` must be kept in sync with mutation-emitting commands. Adding a new write command requires adding a case to `ApplyMutation`. This is a maintenance coupling, but it's explicit (the switch statement) rather than implicit.
- The `CacheStore` interface gains a fourth method, making it slightly wider. Acceptable — the interface is still small (4 methods) and all methods are persistence-related.

### Risks

- **Risk**: A new write command is added but the corresponding `ApplyMutation` case is forgotten, causing silent data loss on replay. **Mitigation**: A test enumerates all mutation-emitting command specs and asserts that `ApplyMutation` handles each one. The test fails at compile time (missing case) or test time (unknown op returned).
- **Risk**: `Raw*` method signatures drift from what `ApplyMutation` expects. **Mitigation**: `ApplyMutation` uses the same `[][]byte` args layout as the command handlers and calls the same methods. Drift would manifest as a compile error.
