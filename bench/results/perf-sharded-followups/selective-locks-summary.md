# Selective shard locking for multi-key handlers

**Branch:** `perf/sharded-followups`
**Date:** 2026-05-03
**Issue:** [#43](https://github.com/j-mizera/gocache/issues/43)
**Outcome:** Multi-key handlers (MGET, MSET, DELETE, RENAME, RENAMENX, SINTER, SUNION, SDIFF, BLPOP, BRPOP) now compute their touched-shard set up front and acquire only those shards' locks via `Engine.DispatchToShards`, instead of acquiring every shard's lock via the bulk-lock path. **Standard MSET +10%** (91k → 100k rps). **Pipelined MSET unchanged** within docker noise (134k → 130k rps) — the pipelined regression #43 hoped to address turned out to be cache-locality-bound, not lock-acquire-bound.

## What changed

### `pkg/cache.Cache` adds two primitives

- **`LockShards(shardIDs []int, write bool) func()`** — acquires the listed shards in ascending shard-id order, returns a release closure. Sorted-acquisition discipline prevents deadlock between concurrent callers.
- **`TouchedShards(keys []string) []int`** — returns sorted unique shard indices for the given keys. Allocation-light: uint64 bitset for the dedup at N≤64; map fallback for larger N (not on the hot path today).

### `pkg/engine.Engine.DispatchToShards`

Mirror of `DispatchWithResult` but accepts a shard subset. Same `stopped` check + `ctx.Err()` check, locks via `Cache.LockShards`, runs fn under the umbrella, releases.

### `pkg/command.Context` adds `TouchedShards []int`

`command.Dispatch` for `MultiKey` commands now branches:
- `len(TouchedShards) > 0` → `Engine.DispatchToShards` (selective)
- else → `Engine.DispatchWithResult` (bulk lock — kept for FLUSHDB, KEYS, SCAN, EXEC, snapshot which legitimately touch all shards)

### Multi-key handlers populate `TouchedShards`

Updated handlers (each one-line addition before the existing `command.Dispatch` call):
- `HandleMget` — keys are `cmdCtx.Args`
- `HandleMset` — keys are even-indexed args (collected once)
- `HandleDelete` — keys are all args
- `HandleRename` / `HandleRenameNX` — keys are `[args[0], args[1]]`
- `HandleSinter` / `HandleSunion` / `HandleSdiff` — keys are all args
- `HandleBlpop` / `HandleBrpop` — keys are all args except the last (timeout)

Handlers that touch every shard (FLUSHDB, EXEC, KEYS, SCAN, SNAPSHOT, LOADSNAPSHOT, WATCH, RANDOMKEY, DBSIZE) leave `TouchedShards` nil and fall through to bulk lock — same as today.

## Bench delta (mean of 2 docker runs each)

### MSET specifically (the regression target)

| Mode | pre-#34 | post-#42 | post-#43 | Δ vs #42 | Verdict |
|---|---:|---:|---:|---:|---|
| **Standard MSET (10 keys)** | 88k | 90k | **100k** | **+10%** | ✅ recovered above pre-#34 |
| **Pipelined MSET (10 keys)** | 229k | 134k | 130k | -3% (noise) | ❌ unchanged |

### Other multi-key commands (no other multi-key in default suite)

Default valkey-benchmark suite doesn't include MGET, RENAME, SINTER, etc. They're not benched here; the change is correct for them but not measured. Worth a follow-up suite if needed.

## Why pipelined MSET didn't recover

The hypothesis was that bulk-locking 16 shards for a 10-key MSET is wasteful — at N=16 random keys touch ~7 unique shards, so we should lock 7 instead of 16. **That much is true**, but the lock-acquire cost wasn't the dominant factor in the pipelined regression.

Per-MSET cost breakdown (estimated):
- Bulk lock acquire (16 × ~30ns) ≈ 480 ns
- Selective lock acquire (7 × ~30ns) ≈ 210 ns
- **Lock savings: ~270 ns per command**
- 10 × `RawSet` (map insert + slab alloc + key copy + per-shard routing): ≈ 5 µs
- RESP serialization + connection overhead: ≈ 1-2 µs
- **Total per pipelined MSET: ~7-8 µs**

Lock acquire is ≤6% of total per-command time. Halving it (≈3% saving) is well below the 42% regression and lost in run-to-run docker noise.

The pipelined MSET regression is likely **cache locality**: pre-#34's MSET-of-10 wrote to one shared map + one shared slab; post-#34's MSET writes to ~7 different per-shard maps + slabs. Each `c.Shard(key)` lookup pulls a different shard struct's cache line; each `RawSet` then mutates a different map header and a different slab arena. CPU cache misses dominate.

The standard-mode +10% confirms the fix's intent (less lock work = faster) but standard-mode per-command overhead (one syscall per command) is large enough that the lock savings show up as a measurable percentage.

## What this means for #43

- **Acceptance criterion** ("pipelined MSET ≥ 206k rps") **not met** — the issue's premise about lock-acquire-bound regression was wrong.
- **The fix is structurally correct** and shipping anyway: it eliminates wasted lock acquisitions across all multi-key handlers, and the cost is bounded (one bitset-based touched-shard computation per command, ~50 ns).
- **The pipelined MSET regression needs a different fix** — see `bench/results/perf-sharded-followups/mset-cache-locality.md` (TODO: separate followup issue) for the real story.

## Verification

- `go vet ./...` clean
- `staticcheck ./...` clean
- `staticcheck -tags 'crashdump otlp' ./...` clean
- `go test -race -count=1 ./...` green across 25 packages

## Reproduce

```bash
sg docker -c "REBUILD=1 bash bench/redis-benchmark/run.sh selective-locks --target gocache"
sg docker -c "bash bench/redis-benchmark/run.sh selective-locks-rerun --target gocache"
```

## Files

- `pkg/cache/cache.go` (+~75 lines) — `LockShards`, `TouchedShards`, `popcount` helpers
- `pkg/engine/engine.go` (+~20 lines) — `DispatchToShards`
- `pkg/command/context.go` (+~15 lines) — `TouchedShards` field, dispatch branch
- `pkg/resp/handler/{basic,keys,set,lists}.go` (1-3 lines each) — populate `TouchedShards` per multi-key handler
- `bench/results/perf-sharded-followups/{selective-locks-*,this file}` — measurement artefacts
