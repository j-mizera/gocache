---
title: Performance
description: Per-shard locking arc — shipped optimizations, measured deltas, and what's still on the table
status: living
last_updated: 2026-06-23
related:
  - Audit-per-shard-arc-summary
  - Audit-go-bench-vs-docker-gap
  - Audit-clientctx-cross-goroutine
  - ADR-0022-modular-performance-budget
  - Server-Architecture
---

# Performance

This page is the single entry point for the gocache performance work. It narrates the per-shard locking arc, links every PR/issue that contributed, points at the audits that quantify what shipped, and lists the levers still on the table. It is the wiki-facing companion to the bench captures under `bench/results/<branch>/`.

The current modular-performance arc is tracked in [Modular Overhead Optimization Plan](modular-overhead-optimization-plan.md) and governed by [ADR-0022](../adr/0022-modular-performance-budget.md): IPC, runtime instrumentation, lifecycle OTLP, and Pub/Sub hot-path features must land within a <=20% modular overhead budget before being called performance-acceptable.

## Why per-shard?

The original `pkg/cache.Cache` was a single map guarded by a single `sync.RWMutex`. Once the sink-aware fast path closed the instrumentation overhead (#26), that one mutex was the dominant write bottleneck on the standard pipelined `redis-benchmark` workload — every command from every connection serialised through it.

Sharding the cache by key splits the map into `N` independent shards, each with its own mutex, items map, slab arena, and LRU. Routing is `shardIndexOf(key) = fnv1a(key) & (N-1)`, so unrelated keys mutate in parallel. Single-key handlers acquire one shard lock; multi-key handlers acquire the sorted set of touched shards (deadlock-free).

The arc spans nine merged PRs plus a measurement audit. It moved gocache from **~0.5×** valkey on pipelined writes to **~0.7–0.95×** depending on workload, with a **−15 % RSS** memory delta as a side effect. This page chronicles each step, what it cost, and what it left behind.

## The arc

| PR / Issue | Branch | What shipped | Measured outcome |
|---|---|---|---|
| [#34](https://github.com/j-mizera/gocache/issues/34) | `arch/per-key-routing` | `Cache` becomes a router over `[]*Shard`; `DispatchToShard` / `DispatchToShards` on the engine; default N=16 | Pipelined writes: ~0.5× → ~0.85× valkey ratio |
| [#43](https://github.com/j-mizera/gocache/pull/43) | `refactor/extract-shard` | `pkg/cache.Shard` extracted to a named type with constructor + methods | Refactor — no perf delta, sets up #44 |
| [#44](https://github.com/j-mizera/gocache/pull/44) | `perf/selective-shard-locking` | Multi-key handlers (MGET, MSET, SINTER, SUNION, SDIFF, …) lock only the touched shard set instead of the global cache | Multi-key wins on workloads where keys hash to a small shard subset |
| [#46](https://github.com/j-mizera/gocache/pull/46) | `perf/selective-shard-locking-multi-key` | `command.Context.TouchedShards` surfaces the touched-shard list to handlers | Threading-only refactor; no perf delta |
| [#47](https://github.com/j-mizera/gocache/pull/47) | `perf/mset-locality-and-bench-gap` | `Shard.BulkSetBytes(pairs)` — pre-sort MSET pairs by destination shard; one bulk-set per shard while the sorted lock set is held | MSET cross-shard fan-out cost reduced; **does not** recover the regression (cause is structural) |
| [#45](https://github.com/j-mizera/gocache/issues/45) | `perf/mset-locality-and-bench-gap` | Build-tag-gated pprof endpoint + Dockerfile `PPROF=1` build-arg + runtime profile rates | Bench gap audit — `runtime.selectgo` cum 18.74 % → 31.30 % in docker; gap is scheduler park/unpark from veth + iptables NAT, **not** syscall cost |
| [#48](https://github.com/j-mizera/gocache/pull/48) | `perf/scale-slab-target-by-shard` | `DefaultTargetSlabBytes / N` per-shard slab target — without this, every shard allocates a full-size slab | RSS at N=16 came down −15 % at 1M keys |
| [#49](https://github.com/j-mizera/gocache/pull/49) | `perf/encoding-and-bufpool` | (closed without merge) reusable `[]byte` bufpool for slab Set/Get + N=4 trial | **Negative result** — bufpool's `sync.Pool` round-trip cost more than the alloc it removed; N=4 lost throughput more than it saved memory |
| [#50](https://github.com/j-mizera/gocache/pull/50) | `perf/lower-default-shard-count` | Default shard count 16 → 8 — N=8 within ~3 % of N=16's throughput optimum on mixed pipelined GetSet, with half the per-shard overhead | Ship default lowered; memory wins keep, throughput essentially flat |

The plugin-isolation refactor that landed alongside the perf arc is tracked separately under [#52 / #53](https://github.com/j-mizera/gocache/pull/53) — it didn't move benchmarks but it's what makes future persistence-as-plugin work possible without dragging server internals into the plugin contract.

## Headline numbers

After every follow-up shipped, on the standardised pipelined `redis-benchmark` workload at the dockerized bench harness (cpuset 0-3, memory 2g):

| Workload | Pre-arc ratio | Post-arc ratio | Notes |
|---|---|---|---|
| Pipelined SET | ~0.5× | ~0.85× | Per-key shard routing |
| Pipelined GET | ~0.5× | ~0.85× | Same |
| Pipelined HSET | ~0.5× | ~0.85× | Single-key |
| Pipelined SADD | ~0.5× | ~1.1× | +119 % from baseline |
| Pipelined LPUSH | ~0.5× | ~0.95× | Single-key |
| Pipelined MSET | ~0.5× | **~0.55×** | Cross-shard cache-line tax (structural) |
| Standard SET | parity | parity | TCP syscall ceiling caps RPS |
| Memory (RSS, 1M keys) | baseline | **−15 %** | Slab arena scaling per shard |

Per-PR captures are stored at `bench/results/<branch>/` with a `summary.md` describing each delta. The taxonomy mirrors the PR table above.

## Re-baselined telemetry results

The telemetry hardening captures were re-baselined after T-BATCH + T-GATE on `perf/telemetry-processing` at commit `f9c3863`. The earlier pipelined numbers were taken at roughly 47% production and are contaminated by that setup, so they should not be compared directly with the post-fix captures.

Post-fix results:

| Mode | IPC | OTel | Notes |
|---|---|---|---|
| Standard (`P=1`) | `SET` 111,731 RPS | `SET` 112,739 RPS | OTel adds ~0% overhead; p99 latency stays around 0.38–0.48 ms |
| Pipelined (`P=10`) | `SET` 854,700 RPS; `GET` 934,579 RPS | `SET` 854,700 RPS; `GET` 1,010,101 RPS | Throughput stays competitive, but telemetry skip rate does not materially improve |

Skip counters remained roughly unchanged at ~53% despite the 5× operation-count reduction (`1.5M -> 300K` attempted operations): IPC `skipped_operations=165,551` / `operation_completed=134,651` / `total=300,202` (~55% skipped); OTel `skipped_operations=156,630` / `operation_completed=143,576` / `total=300,206` (~52% skipped). The 8 shards still show sustained exhaustion, roughly evenly distributed at ~19K–22K per shard.

Memory stayed comparable to the pre-fix shape: IPC `delta_rss=232MB` (61MB -> 293MB) and OTel `delta_rss=205MB` (63MB -> 268MB), versus the pre-fix ~+204MB profile. T-GATE's partial cut reduced some per-command work, but `copyOperationContext` still runs, so RSS did not materially drop.

T-PARALLEL (per-shard drain workers, `workerCount=2`) was then implemented and stress-tested. The mechanics were correct: serial parity held, concurrent dispatch did not double-drain, and 138 tests passed under `-race`. But the benchmark result was negative in the thesis sense: pipelined `skipped_operations` only moved from `165,551` pre-T-PARALLEL to `162,532` after it, so the skip rate stayed effectively unchanged at ~54%. Throughput stayed comparable — `SET` 793K pipelined vs 854K pre, `GET` 980K vs 934K — so the change did not hurt the fast path, but it also did not reduce telemetry loss.

Root cause: on the 4-pinned-core target, the drain is CPU-bound. Adding drain workers splits the same saturated CPU budget across more goroutines; it does not create more recycling capacity. The bottleneck is per-op drain cost (`copyOperationContext` still always runs because the T-GATE hoisted check was removed for replay retention) plus CPU contention with serving goroutines.

Conclusion: parallelizing the drain is architecturally sound — correct, tested, and serial-parity preserving — but insufficient on a CPU-saturated target. The telemetry loss is now bounded, counted, and visible (FR-001), which is the architectural achievement. Zero-drop pipelined telemetry would require either more CPU cores or a cheaper per-op drain path, likely by recovering the `copyOperationContext` skip via a replay-safe interest gate.

## Audits

The arc produced three audit documents that are the source of truth for thesis-grade claims about *why* a given delta exists:

- **[Per-shard arc summary](Audit-per-shard-arc-summary)** — the long-form thesis anchor. Walks through every shipped PR, the structural caps (MSET regression, cross-shard cache-line tax, default-deployment memory cost), and what's still on the table.
- **[Go-vs-docker bench gap](Audit-go-bench-vs-docker-gap)** — pprof-attributed investigation of why Go bench shows +18 % pipelined-GET while docker shows +3 % on the same code. Conclusion: the gap is `runtime.selectgo` arbitration from docker's veth + iptables NAT, not syscall cost. Future arcs should report both tiers.
- **[clientctx cross-goroutine race audit](Audit-clientctx-cross-goroutine)** — surfaced from #32, but methodologically tied to the per-shard work because per-shard locking changes which goroutine writes which field. Sweeps every `ClientContext` field for cross-goroutine read/write hazards. No new races found beyond the one #32 fixed.

Audits are intentionally not folded into a "Performance" group — an audit can relate to anything (perf, design, security, race conditions). The three above happen to all fall under the perf arc, but the category is kept separate.

## What's structural (won't be recovered)

Some costs are the price of the architecture and won't come back without un-sharding:

- **MSET regression −31 %** vs the single-mutex baseline. Every MSET spans multiple shards; each shard touched costs a cache-line bounce on the shared per-shard lock plus per-shard book-keeping (LRU update, used-bytes, eviction check). The single-mutex baseline pays one of each per command; sharding pays N. Recovering this would require fundamentally different sharding (RCU, transactional memory, COW) — out of scope for the thesis.
- **Cross-shard cache-line tax** on any K-shard handler — pays K-times-shard-overhead vs a single-mutex implementation paying once. Single-key handlers (the overwhelmingly common case) win because K=1 < N; multi-key handlers' break-even depends on key distribution.
- **Default-deployment memory cost** — even with #48's slab scaling, N shards each carry their own maps, channel pools, and slab metadata. At N=8 the baseline RSS is ~10× valkey's at 1M keys. Most of that is the persistence layer (gob snapshot worker + handlers), and is the target of the upcoming persistence-as-plugin arc.
- **Bench tier disagreement** — Go bench and docker bench will disagree by 5–10× in relative-percentage terms. This is the nature of the measurement, not a bug. Acceptance criteria denominated in docker rps must be measured in docker; Go bench can't substitute. See the bench-gap audit for the pprof-attributed write-up.

## What's still on the table

The active performance priority is now the modular overhead arc: reduce IPC, runtime instrumentation, lifecycle OTLP, and Pub/Sub regressions without giving up the plugin-isolation model. The sequence is captured in [Modular Overhead Optimization Plan](modular-overhead-optimization-plan.md): per-plugin FIFO writer loop, event-only runtime instrumentation, internal attribution harness, GCPC stream topology evaluation, GCPC allocation/correlation cleanup, then Pub/Sub push batching or a specialized built-in data-plane if generic push remains over budget. The first runtime instrumentation slice now ships traces/logs through the existing event/logcollector path; broader log-transport or traffic-class changes remain benchmark-gated. For diagnosis runs, `BENCH_STATS=1` on `bench/redis-benchmark/run-ipc.sh` enables `pkg/benchstats` and the benchmark-only `plugins/benchprobe` IPC plugin, producing startup/standard/pipelined JSON snapshots beside the CSV/RSS files. The async event IPC measurement kickoff is tracked in [Async Event IPC Phase 1A](async-event-ipc-phase-1a.md).

Older command-flow levers remain separate from the modular-overhead work:

- **#27 engine pooling** — still a plausible allocation-pressure lever, but it should be re-benchmarked after the plugin path is no longer the dominant regression.
- **#28 read-lock bypass** — not an active plan. Later experiments showed the cache-wide read-lock bypass/mode-switching route was a negative result; do not retry it without new runtime or workload evidence.
- **#29 batched pipelined dispatch** — still decision-gated and only worth pursuing if pipelined-write headroom remains >=30% after higher-confidence fixes land.

## Lessons

1. **Always report two tiers of bench numbers** — Go in-process (`go test -bench`) for hot-path attribution and docker (`valkey-benchmark` against the published image) for end-to-end. They will disagree by 5–10× and that's expected.
2. **Negative results matter** — #49 burnt three days but the writeup tells future contributors not to retry the bufpool path or N=4 default unless something fundamental changes.
3. **Memory is just as much a thesis line as throughput** — #48's −15 % RSS without a throughput cost is a clean win that wouldn't have happened without explicit profiling of arena overhead.
4. **The bench gap audit is reusable** — any future "why does docker disagree with Go bench" question now has a writeup at `Audit-go-bench-vs-docker-gap` and a pprof toggle to attach a profiler in production-shape builds.

## Telemetry hardening captures

The post-hardening IPC captures live under `bench/results/perf-telemetry-processing/`:

- `telemetry-post-hardening-20260611-*` — diagnostic benchmark runs. They are useful regression artifacts, but they are not telemetry-visible through the harness, so they do not prove `metrics.telemetry` visibility.
- `telemetry-post-hardening-telemetry-visible-20260612-gocache-ipc-*` — current FR-004.1 visibility baseline. These benchmark reruns include baseline, standard, and pipelined `/telemetry` JSON snapshots with `telemetry.*` keys, using least-privilege config grants for `server:query:metrics.commands` and `server:query:metrics.telemetry` without the Prometheus `events` scope.
- `telemetry-post-hardening-telemetry-visible-20260611-gocache-ipc-*` — earlier telemetry-visible rerun, superseded by the least-privilege 20260612 rerun.

The `/telemetry` JSON snapshots are diagnostic visibility evidence, not standalone performance-result claims or final Phase 2/3 behavior evidence.

## Pointers

- Bench captures: `bench/results/<branch>/` per PR (in the repo)
- Architecture diagrams: [Server: Component Diagrams](Server-Components-Diagrams), [Server: Sequence Diagrams](Server-Sequence-Diagrams)
- Solution architecture: [Server-Architecture](Server-Architecture)
- Roadmap and merge state: [Server Roadmap](Server-Roadmap) (wiki) and the merged-PRs feed on the repo's `main` branch
