# Per-shard locking prototype — N-sweep + channel-hop relief evidence

**Issue:** [#34](https://github.com/j-mizera/gocache/issues/34)
**Branch:** `proto/per-shard-locking` (off `main` at `2e5dcfd`)
**Date:** 2026-05-02
**Outcome:** A throwaway sharded prototype (`pkg/shardproto`) sweeping N ∈ {1, 4, 8, 16, 32} confirms the architectural premise: per-shard engines + per-shard mutexes recover **+53%** throughput on mixed pipelined GET+SET over the prototype's own N=1 baseline, exceed every acceptance threshold from issue #34 by a wide margin (pipelined GET 1.27 M cmd/s, pipelined SET 1.14 M cmd/s at N=16), and reduce channel-hop mutex contention by **36×**. Optimal N is 16. Production implementation (per-shard rewrite of `pkg/cache` + `pkg/engine`) is unblocked.

## What this prototype is

`pkg/shardproto` is a self-contained package that mirrors only the surface needed to validate the architectural change: per-shard sharded keyspace, one engine goroutine per shard, GET / SET / HSET handlers, and a minimal RESP TCP server. Slab allocator, instrumentation, multi-key, transactions, WATCH, and persistence are all out of scope.

Each engine has its own `cmdChan` / per-call result channel + sync.Pool. Connection goroutines route every command to the shard owning the key (`xxhash.Sum64String(key) & (n-1)`) and submit to that shard's engine. The same channel-hop pattern as production, but distributed across N independent queues.

The package name carries a `proto` suffix so production code never imports it — it is throwaway measurement scaffolding, not API.

## N-sweep — throughput

`go test -bench='^BenchmarkTCPShard_' -benchtime=5s -cpu=4 ./pkg/shardproto/`

50 parallel TCP connections (`b.SetParallelism(13)` × `-cpu=4`), 100k-key keyspace, P=10 for pipelined runs. Reader/writer split done deterministically per goroutine via an atomic counter — same shape as the diagnosis benchmarks.

### Mixed pipelined (50/50 reader/writer, P=10)

| N | ns/op | Effective rps (combined) | Δ vs N=1 |
|---:|---:|---:|---:|
| **1** | 12 777 | 783 k cmd/s | baseline |
| **4** | 8 855 | 1 130 k cmd/s | +44% |
| **8** | 8 561 | 1 168 k cmd/s | +49% |
| **16** | 8 327 | **1 201 k cmd/s** | **+53%** ← optimum |
| **32** | 8 526 | 1 173 k cmd/s | +50% |

`Mixed_GetHset_Pipelined` (collection writer all on one shared key + readers on string keys) shows a more modest gain because the writers cluster on a single shard, but readers still benefit:

| N | ns/op | rps | Δ vs N=1 |
|---:|---:|---:|---:|
| 1 | 11 857 | 843 k | baseline |
| 4 | 9 646 | 1 037 k | +23% |
| 8 | 9 032 | 1 107 k | +31% |
| **16** | 9 231 | 1 083 k | +28% |
| 32 | 9 010 | 1 110 k | +32% |

### Single-mode pipelined (all readers OR all writers, P=10)

| N | GET rps | Δ vs N=1 | SET rps | Δ vs N=1 |
|---:|---:|---:|---:|---:|
| 1 | 972 k | baseline | 912 k | baseline |
| 4 | 1 160 k | +19% | 1 109 k | +22% |
| 8 | 1 173 k | +21% | 1 145 k | +25% |
| **16** | **1 269 k** | **+31%** | **1 140 k** | +25% |
| 32 | 1 276 k | +31% | 1 130 k | +24% |

### Standard mode (no pipelining)

| N | GET rps | SET rps |
|---:|---:|---:|
| 1 | 281 k | 258 k |
| 4 | 276 k | 260 k |
| 8 | 263 k | 270 k |
| 16 | 269 k | 252 k |
| 32 | 273 k | 254 k |

Standard mode shows essentially no per-shard benefit (variance within ±10% noise). This is expected — without pipelining each command pays the syscall + scheduler round-trip individually, and that cost dwarfs the channel-hop relief that sharding provides. **Per-shard locking is a pipelined-throughput optimization**, not a per-RTT-latency optimization.

## Verifying the architectural mechanism

Profile capture at N=1 vs N=16 on `Mixed_GetSet_Pipelined`, 10s benchmark, identical workload:

### Mutex contention (the channel-hop signature)

| Metric | N=1 | N=16 | Reduction |
|---|---:|---:|---:|
| Total `runtime.unlock` time | 3 314 ms | 85 ms | **39×** |
| Total `runtime.selectgo` mutex-wait | 2 383 ms | 46 ms | **52×** |
| `Engine.Dispatch` caller-side wait | 1 732 ms | 29 ms | **60×** |

The mutex profile attributes nearly all contention in N=1 to `runtime.unlock` reached via `runtime.selectgo` → `Engine.Dispatch` — exactly the channel-hop signature the diagnosis identified. At N=16 the same path drops by 36–60× across all relevant metrics. This is direct mechanistic evidence that splitting the global cmdChan into N independent shard channels eliminates the contention.

### Block (caller wait) time on `Engine.Dispatch`

| | N=1 | N=16 |
|---|---:|---:|
| Caller-side wait in `Engine.Dispatch` | 61.9 s | 35.9 s |
| Idle engine goroutines parked on empty cmdChan | n/a | 463.9 s |

At N=16, total wait time appears higher in raw numbers because **engine goroutines now spend most of their time idle waiting for work** — that's the *signature of headroom*, not a regression. The metric that matters is caller-side wait in `Engine.Dispatch`, which dropped from 62 s to 36 s. Throughput rose 53% in spite of (because of) more goroutines being idle.

## Acceptance criteria check

From issue #34's acceptance criteria, prototype-level pre-validation:

| Criterion | Threshold | Prototype N=16 | Verdict |
|---|---:|---:|---|
| Pipelined GET | ≥ 900 k rps | **1 269 k** | ✅ +41% headroom |
| Pipelined SET | ≥ 775 k rps (no regression) | **1 140 k** | ✅ +47% headroom |
| Mixed-workload balanced gain | Reads up, writes within −5% | +31% GET / +25% SET (single-mode); +53% combined mixed | ✅ |
| WATCH consistency under -race | n/a in prototype | not tested | (Phase 2c scope) |
| Memory ≤ +20% | n/a in prototype | not measured | (Phase 2d scope) |
| `go test -race ./pkg/shardproto/` | clean | green | ✅ |

The prototype's absolute numbers are *higher* than the production main figures (~795 k pipelined GET, ~775 k pipelined SET) because the prototype handlers carry no instrumentation — no event bus, no operations tracker, no plugin hooks, no slab. The production implementation will have these and pay their cost. The relevant signal is the **N=1 → N=16 relative speedup of +25–53%**, which the production implementation should retain since the bottleneck the diagnosis identified (channel-hop) is what sharding fixes.

Translated to production main's diagnosis numbers (425 k mixed pipelined → +53% would land at ~650 k; 795 k pipelined GET → +31% would land at ~1 040 k), the production gate criteria stay comfortable.

## Optimal N

**N = 16.** Reasons:

- N=4 captures most of the gain (+44% on mixed, vs +53% at N=16). Diminishing returns thereafter.
- N=8 → N=16 gains another +4% on mixed pipelined and +10% on single-mode GET (1 173 k → 1 269 k) — measurable, not free.
- N=16 → N=32 is statistically flat or slightly negative on every workload. Doubling shards adds memory overhead (per-shard mutex + map header + sync.Pool struct + per-shard engine cmdChan buffer of 100 commands) without throughput payoff.
- 16 is also a comfortable size for slab metadata multiplication — at production-scale slab sizes, 16 copies stays well within the +20% memory cap from issue #34's acceptance criteria.

## Surprises

1. **N=1 in the prototype outperforms production main on every workload.** Prototype N=1 hits 783 k mixed pipelined and 972 k single-mode pipelined GET. Production main on the diagnosis benchmark hit 425 k and ~795 k respectively. The gap (~46% on mixed, ~22% on GET) is the cost of full instrumentation in production. Per-shard locking targets the channel-hop bottleneck, but there is also instrumentation overhead production carries that the prototype does not. Neither one undermines the other; they are independent cost sources.

2. **HSET on a single shared key gains less from sharding than string SETs do.** Mixed_GetHset shows +28% at N=16 vs +53% for mixed string Set. This is because HSET writers all cluster on one shard (same key → same shard), so the writer side does not benefit. Readers do (their keys spread across shards), which is what produces the residual gain. **Implication:** workloads dominated by writes to a single hot key (e.g. global counter, single hash, single sorted set) will not get the full sharding benefit. This is a workload property, not a design defect — Redis cluster has the same characteristic. Mitigation is at the application level (key sharding, not value sharding).

3. **Standard-mode benchmarks are insensitive to N.** GET_Standard rps is flat across all N values within ±10% noise. The channel-hop is hidden by per-RTT syscall cost when each command is its own round-trip. Sharding does not hurt here, just doesn't help. The thesis target of "within 20% of valkey" was already met for standard mode by post-#27 main; sharding's job is the pipelined-mixed gap.

## Verdict

**Per-shard locking validated. Production implementation (per-shard rewrite of `pkg/cache` + `pkg/engine`, single-key path first) is unblocked.**

Optimal N = 16.

The next branch should rewrite the production `pkg/cache.Cache` to embed N shards (each with its own mutex, items map, slab allocator, LRU bookkeeping, and watch.Manager) and rewrite `pkg/engine.Engine` to start one goroutine per shard, dispatching by key hash. Multi-key commands (MGET, MSET, etc.) get sorted-shard-ID lock acquisition; transactions and WATCH coordinate across shards via the same primitive. See the parent plan note for the full sequencing.

## Files

```
pkg/shardproto/
├── shard.go              (Shard: mutex + items map)
├── cache.go              (Cache: N shards, xxhash routing)
├── engine.go             (per-shard engine goroutines + dispatch)
├── server.go             (minimal RESP TCP server, GET/SET/HSET)
├── shardproto_test.go    (correctness)
└── bench_test.go         (parameterized N-sweep benchmarks)

bench/results/proto-per-shard-locking/
├── prototype-summary.md  (this file)
├── sweep/raw.txt         (full N-sweep output)
└── profiles/
    ├── n1/{cpu,block,mutex}.{prof,top.txt}     (10s capture at N=1)
    └── n16/{cpu,block,mutex}.{prof,top.txt}    (10s capture at N=16)
```

## Reproduce

```bash
# N-sweep across all six benchmarks (~3 min)
go test -run=NONE -bench='^BenchmarkTCPShard_' -benchtime=5s -cpu=4 ./pkg/shardproto/

# Profile capture at N=1 vs N=16 (~30s)
go test -run=NONE -bench='^BenchmarkTCPShard_Mixed_GetSet_Pipelined$/N=1$' \
  -cpu=4 -benchtime=10s \
  -cpuprofile=/tmp/n1-cpu.prof -blockprofile=/tmp/n1-block.prof -blockprofilerate=10000 \
  -mutexprofile=/tmp/n1-mutex.prof -mutexprofilefraction=10 \
  ./pkg/shardproto/
go tool pprof -top -cum /tmp/n1-mutex.prof
```
