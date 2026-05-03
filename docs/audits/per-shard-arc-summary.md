---
title: Per-shard arc summary
description: Thesis anchor — full chronicle of the per-shard locking arc, shipped PRs, structural caps, and remaining levers
status: living
last_updated: 2026-05-03
related:
  - Performance
  - Audit-go-bench-vs-docker-gap
  - Audit-clientctx-cross-goroutine
  - Server-Architecture
---

# Per-shard locking arc — summary

This is the thesis-anchor write-up for the per-shard locking arc that landed across issues #34 and follow-ups #43, #44, #46, #47, #48, #49, #50, plus the bench-methodology audit #45. It documents what was tried, what shipped, what didn't, and what's structurally bounded — so future readers know which gaps are still on the table and which are the price of the architecture.

## Goal

The single-mutex `Cache` was the dominant pipelined-write bottleneck once the sink-aware fast path (#26) closed the instrumentation overhead. Sharding the cache by key (FNV-1a → mask) lets unrelated keys mutate in parallel without a global write lock.

## Headline result

After every follow-up shipped, the gocache vs valkey ratio on the standardised pipelined redis-benchmark moves from **~0.5×** (pre-arc) to **~0.7×–0.95×** depending on workload:

| Workload | Pre-arc ratio | Post-arc ratio | Notes |
|---|---|---|---|
| Pipelined SET | ~0.5× | ~0.85× | Per-key shard routing |
| Pipelined GET | ~0.5× | ~0.85× | Same |
| Pipelined HSET | ~0.5× | ~0.85× | Single-key |
| Pipelined SADD | ~0.5× | ~1.1× | +119% from baseline |
| Pipelined LPUSH | ~0.5× | ~0.95× | Single-key |
| Pipelined MSET | ~0.5× | **~0.55×** | Cross-shard cache-line tax |
| Standard SET | parity | parity | TCP syscall ceiling caps RPS |
| Memory (RSS, 1M keys) | baseline | **−15%** | Slab arena scaling per shard |

Memory and RSS numbers measured at N=8 (the ship default) on the dockerized harness. Full per-PR captures live under `bench/results/` (taxonomy in [Bench layout](#bench-layout) below).

## What shipped

### #34 — per-key shard routing (`arch/per-key-routing`)
Initial cut: `Cache` becomes a router over `[]*Shard`; each shard has its own mutex, items map, slab arena, LRU list. `shardIndexOf(key) = fnv1a(key) & (N-1)`. Engine grows `DispatchToShard(ctx, shardIdx, fn)` and `DispatchToShards(ctx, shardIDs, fn)`. Single-key handlers route to one shard; multi-key handlers compute `cache.TouchedShards(keys)` and acquire the sorted shard set (deadlock-free).

Default N=16 at first.

### #43 — `pkg/cache.Shard` extraction (`refactor/extract-shard`)
Moved per-shard state from inline anonymous struct into a named `Shard` type with its own constructor and methods. Pure refactor; sets up the surface for #44.

### #44 — selective shard locking for multi-key handlers (`perf/selective-shard-locking`)
Multi-key handlers (MGET, MSET, SINTER, SUNION, SDIFF, …) now lock only the touched shard set instead of taking the full cache lock. Net win on workloads where keys hash to a small number of shards.

### #46 — multi-key locking via `command.Context.TouchedShards` (`perf/selective-shard-locking-multi-key`)
Surface the touched-shard list on `command.Context` so handlers don't recompute it. Threading-only refactor; the locking strategy is the same as #44.

### #47 — MSET shard-batched bulkSet (`perf/mset-locality-and-bench-gap`)
`Shard.BulkSetBytes(pairs []BulkPair)` pre-sorts pairs by destination shard inside `HandleMset`, then issues one bulk-set call per shard while the sorted shard-lock set is already held. Saves cross-shard fan-out overhead but **does not recover the MSET regression** — the residual cost is structural cache-line bounce, not algorithmic.

### #45 — Go-vs-docker bench gap audit (`perf/mset-locality-and-bench-gap`)
Build-tag-gated pprof endpoint (`-tags pprof`) added to the docker image; `Dockerfile`'s `PPROF=1` build-arg publishes port 6060. Runtime profile rate setup (`SetBlockProfileRate(10000)`, `SetMutexProfileFraction(10)`).

Measured: docker bench shows `runtime.selectgo` cumulative jumping from 18.74 % (Go bench) → 31.30 % (docker bench), and `Syscall6` flat dropping 25.93 % → 19.47 %. Conclusion: the gap is scheduler park/unpark from docker's veth + iptables NAT, **not** syscall cost. Future arcs should report both Go-bench and docker-bench numbers — 5–10× absolute disagreement is the nature of measurement, not a bug.

Full audit: `docs/audits/go-bench-vs-docker-gap.md`.

### #48 — slab target scales by shard count (`perf/scale-slab-target-by-shard`)
`DefaultTargetSlabBytes / N` per-shard slab target. Without this, every shard allocates a full-size slab and the total arena footprint scales linearly with N — at N=16 we'd doubled RSS for no win. With scaling, RSS at N=16 came down 15 %.

### #49 — bufpool experiment + N=4 trial (negative result, `perf/encoding-and-bufpool`)
Tried (a) reusable `[]byte` buffer pool for slab `Set`/`Get` and (b) lowering N to 4 to reduce per-shard arena overhead further. Both **regressed vs N=8 + no bufpool**: the bufpool path added a hot-loop sync.Pool round-trip that cost more than the alloc it removed; N=4 lost throughput on multi-shard workloads more than it gained on single-shard. Branch closed without merge. Memory note: [49-followup-bufpool-n4-negative](../../projects/gocache/memory/49-followup-bufpool-n4-negative.md) in Obsidian.

### #50 — default shard count 16 → 8 (`perf/lower-default-shard-count`)
The N-sweep prototype showed N=8 within ~3 % of N=16's throughput optimum on mixed pipelined GetSet, while halving the per-shard memory overhead (slab arenas, maps, channel pools, slab metadata). Ship default lowered.

## What's structural (won't be recovered without un-sharding)

### MSET regression
Pipelined MSET is **−31 %** vs the single-mutex baseline even after #47. Cause: every MSET spans multiple shards; each shard touched costs a cache-line bounce on the shared per-shard lock plus per-shard book-keeping (LRU update, used-bytes update, eviction check). The single-mutex baseline pays one of each per command; sharding pays N. This is the price of the design and would only come back via a fundamentally different sharding strategy (RCU, transactional memory, copy-on-write, …) that is out of scope for the thesis.

### Cross-shard cache-line tax
The same effect generalises. Any handler that touches K shards pays K-times-shard-overhead vs a single-mutex implementation paying once. Single-key handlers (the overwhelmingly common case) win because K=1 < N; multi-key handlers' break-even depends on workload key distribution.

### Default-deployment memory cost
Even with #48's slab scaling, N shards each carry their own maps, channel pools, and slab metadata. At N=8 the baseline RSS is ~10× valkey's at 1M keys — most of which is the persistence layer (gob snapshot worker + handlers; coordinated for replacement in [persistence-as-plugin](../../projects/gocache/plans/persistence-as-plugin.md)).

## What's still on the table (future levers, not in this arc)

Tracked under the [command-flow optimization plan](../../projects/gocache/plans/command-flow-optimization.md):

- **#27 engine pooling** — pool `make(chan any, 1)` resChan + `*command.Context` allocations. Profile-attributed projection: +10–15 % uniform.
- **#28 read-lock bypass** — classify commands as read-only, drop LRU-list mutation on read path, run read handlers inline under `cache.RLock()`. Projection: pipelined GET → ~0.85× valkey.
- **#29 batched pipelined dispatch** — single-lock-acquisition for batched same-key pipelined writes. Decision-gated; only if pipelined-write headroom remains ≥30 % after #27/#28.

## Bench layout

Per-branch captures live under `bench/results/<branch-name>/` so the diff is clean per-PR. The taxonomy:

```
bench/results/
├── arch-per-key-routing/        ← #34 baseline + per-key routing
├── refactor-extract-shard/      ← #43 pure-refactor parity check
├── perf-arc-followups/          ← #44/#46 selective shard locking
├── perf-mset-locality-and-bench-gap/ ← #47 MSET bulkset + #45 bench gap audit
├── perf-memory-reduction/       ← #48 slab target scaling
├── perf-memory-reduction-combined/  ← combined N=8 + slab scaling
├── perf-sharded-followups/      ← #50 default-N change validation
├── perf-engine-pooling/         ← #27 (planned)
├── perf-read-lock-bypass/       ← #28 (planned)
├── perf-sink-aware-fast-path/   ← #26 (already shipped)
├── feat-memory-optimization/    ← cross-arc memory writeup
├── fix-issues-23-24/            ← diagnosis baseline
├── diag-per-shard-locking/      ← prototype N-sweep
└── proto-per-shard-locking/     ← prototype before refactor
```

Each branch directory contains `*-gocache.csv`, `*-gocache-pipelined.csv`, optional `*-gocache-memory.txt`, and a `summary.md` describing the deltas.

## Lessons

1. **Always report two tiers of bench numbers**: Go in-process (`go test -bench`) for hot-path attribution and docker (`valkey-benchmark` against the published image) for end-to-end. They will disagree by 5–10× and that's expected.
2. **Negative results matter**: #49 burnt three days but the writeup tells future-us not to retry the bufpool path or N=4 default unless something fundamental changes. Memory note exists for this reason.
3. **Memory is just as much a thesis line as throughput**: #48's −15 % RSS without a throughput cost is a clean win that would not have happened without explicit profiling of arena overhead.
4. **The bench gap audit (#45) is reusable**: any future "why does docker disagree with Go bench" question now has a writeup at `docs/audits/go-bench-vs-docker-gap.md` and a pprof toggle to attach a profiler in production-shape builds.

## Pointers

- Thesis writeup: this file plus `bench/results/perf-mset-locality-and-bench-gap/summary.md` and `docs/audits/go-bench-vs-docker-gap.md`.
- Plan: [command-flow-optimization](../../projects/gocache/plans/command-flow-optimization.md), child plan [per-shard-locking](../../projects/gocache/plans/command-flow/per-shard-locking.md) (Obsidian).
- Postmortem (Obsidian memory): `per-shard-arc-postmortem`.
