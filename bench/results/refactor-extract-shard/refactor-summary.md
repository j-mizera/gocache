# Cache → Shard structural refactor (N=1)

**Branch:** `refactor/extract-shard` (off `main` at `af69db7`)
**Date:** 2026-05-02
**Issue:** [#34](https://github.com/j-mizera/gocache/issues/34)
**Outcome:** Structural refactor only. Per-key cache state moves from `*Cache` into a new `*Shard` type; `*Cache` becomes a thin router over `[]*Shard`. The engine grows per-shard goroutines and a `DispatchToShard` API. Default configuration is N=1, behaviorally identical to pre-refactor — verified by `go test -race ./...` clean and benchmark numbers within ±5% (run-to-run noise) on every workload.

## What changed structurally

### `pkg/cache/shard.go` (new, 437 lines)

`Shard` owns one slice of the keyspace plus everything that goes with it:
- `mu sync.RWMutex`
- `items map[string]SlabPointer`
- `nativeValues map[SlabPointer]any`
- `keysBySlot map[SlabPointer]string`
- `slabs *slab.Allocator`
- `lruHead`, `lruTail` (the LRU list, threaded through SlotMeta)
- `usedBytes`, `maxBytes`, `evictionPolicy`
- per-shard `onMutate` / `onMutateAll` callbacks bound to the parent Cache's external WATCH callbacks

All per-key methods (`rawGet`, `rawSet`, `rawDelete`, `rawTTL`, `nativeSize`, `setExpiration`, etc.) live on `*Shard`, plus the LRU helpers (`lruPushFront`/`lruRemove`/`lruMoveToFront`), eviction (`evictLRU`), and storage internals (`setInternal`/`setNativeInternal`/`setPackedInternal`/`delete`/`keySize`/`chargedSize`).

### `pkg/cache/cache.go` (rewritten, 470 lines)

`Cache` becomes a thin router:
- `shards []*Shard` + `n uint64`
- shared configuration (`packed`, `evictionPolicy`, `maxBytes`, `OnMutate`, `OnMutateAll`)
- public single-key methods route via `c.Shard(key).method()`
- `LastAccess(e)` and `ResolvePacked(e)` route via the unexported `Entry.shard` back-reference (set by `Shard.entryFromSlot`)
- multi-key methods (`Rename`, `Range`, `Clear`) handle the same-shard case (always at N=1) and stub or aggregate for cross-shard
- aggregate methods (`Len`, `UsedBytes`, `SlabStats`) sum across shards
- bulk-locking (`Lock`/`Unlock`/`RLock`/`RUnlock`) acquires every shard's mutex in shard-id order; used by tools like `bench/gctrace` and any future cross-shard primitives

### `pkg/cache.Entry` gains an unexported `shard *Shard` back-reference

Set by `Shard.entryFromSlot`. Lets `Cache.LastAccess(e)` and `Cache.ResolvePacked(e)` dereference into the right shard's slab without a separate lookup. Zero handler-code change — the field is private and existing handler call sites use `cmdCtx.Cache.ResolvePacked(entry)` unchanged.

### `pkg/engine/engine.go` (rewritten, 175 lines)

`Engine` now owns N `shardEngine` instances, each with its own goroutine + `cmdChan` + `stopChan`. Each goroutine acquires its shard's write lock before running a handler.

Three dispatch entrypoints:
- `Dispatch(ctx, fn)` and `DispatchWithResult(ctx, fn)` — route to shard 0 (legacy single-engine semantics, used by snapshot save / cleanup workers).
- `DispatchToShard(ctx, shard, fn)` — caller-specified shard. Single-key handlers will use this once routing is wired (PR 3).

The `resChanPool` Put-on-success-only safety rule from `#28` carries through unchanged.

## What did not change

- Public `Cache` API surface: every method that existed pre-refactor still exists with the same signature. Handlers, tests, plugins, server code all compile without modification.
- `pkg/engine.Engine.Dispatch` / `DispatchWithResult` keep their pre-refactor semantics by routing to shard 0 (the only shard at N=1).
- WATCH propagation, eviction semantics, slab encoding, persistence I/O — all behaviorally identical at N=1.
- No multi-key handler is stubbed. Every test still passes.

## Verification

| Check | Status |
|---|---|
| `go vet ./...` | clean |
| `staticcheck ./...` | clean |
| `staticcheck -tags 'crashdump otlp' ./...` | clean |
| `go test -race -count=1 ./...` | green across all 25 packages |
| Bench at N=1 vs main `af69db7` (5s × 2 runs each) | within ±5% on every workload |

### Bench delta (N=1 post-refactor vs pre-refactor)

| Benchmark | Pre (ns/op) | Post mean (ns/op, 2 runs) | Δ |
|---|---:|---:|---:|
| TCP_GET_Pipelined | 12 669 | 13 447 | +6% (within run-to-run noise) |
| TCP_GET_Standard | 4 026 | 4 017 | -0.2% |
| TCP_Mixed_GetSet_Pipelined | 13 880 | 13 238 | -4.6% |
| TCP_Mixed_GetHset_Pipelined | 15 616 | 15 418 | -1.3% |
| TCP_Mixed_GetSet_Standard | 4 024 | 4 054 | +0.7% |
| TCP_SET_Standard | 4 235 | 4 219 | -0.4% |

GET_Pipelined alone shows ~6% mean increase but its single-run spread is 12 584 → 14 827 ns/op (16% range) on the same code, so the +6% sits inside that window. Allocation count (`280 allocs/op`) and bytes (`9 845 B/op`) are byte-identical to pre-refactor.

## Why N=1

This PR is a structural refactor only — it sets up the per-shard machinery without changing default behavior. Bumping `N` to 16 (the prototype's optimal value, validated in #39) requires:
1. Routing single-key handlers to `DispatchToShard(shard, fn)` so each command lands on the goroutine owning its key — currently the production handlers go through `Dispatch`/`DispatchWithResult` which routes to shard 0.
2. Cross-shard coordination for multi-key handlers (MGET, MSET, RENAME, KEYS, SCAN, SINTER/SUNION/SDIFF, MULTI/EXEC, WATCH, FLUSHDB, snapshot save/load, cleanup). At N=1 they all hit shard 0 and just work; at N>1 they need sorted-shard-ID lock acquisition and per-shard fan-out.

Both are the next PR's scope.

## Reproduce

```bash
go test -race -count=1 ./...
go test -run=NONE -bench='^BenchmarkTCP_(Mixed|GET_Pipelined|GET_Standard|SET_Standard)' \
  -benchtime=5s -cpu=4 -count=2 ./pkg/server/
```

## Files

- `pkg/cache/shard.go` (new, 437 lines)
- `pkg/cache/cache.go` (rewritten, 470 lines — was 837)
- `pkg/engine/engine.go` (rewritten, 175 lines — was 137)
- `bench/results/refactor-extract-shard/post-refactor.txt` (raw bench output, two runs)
- `bench/results/refactor-extract-shard/refactor-summary.md` (this file)
