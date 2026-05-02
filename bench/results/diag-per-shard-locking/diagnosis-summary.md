# Mixed-workload contention diagnosis (per-shard locking)

**Issue:** [#34](https://github.com/j-mizera/gocache/issues/34)
**Branch:** `diag/per-shard-locking` (off `main` at `bc563e6`)
**Date:** 2026-05-02
**Methodology:** Required mixed-workload pass before any sync-primitive-changing plan ships, per the post-#28 lesson documented in `projects/gocache/plans/command-flow/diagnosis-pre-implementation.md` "Lessons learned (post-#28)" section.
**Outcome:** Channel-hop scheduler cost is the dominant contention source under mixed read+write load (49–67% of mutex contention, 64–72% of block time). `cache.mu` contention is essentially absent (< 5% of mutex contention). Per-shard locking — N independent shard channels + N independent shard mutexes — is the architectural lever that addresses this. The diagnosis confirms the premise of issue #34 and unblocks prototyping.

## What this pass measures

Three concurrent-load benchmarks were added to `pkg/server/bench_test.go`:

| Benchmark | Half goroutines run | Other half run | Pipeline |
|---|---|---|---|
| `BenchmarkTCP_Mixed_GetSet_Pipelined` | GET on preloaded keys | SET on same keys | P=10 |
| `BenchmarkTCP_Mixed_GetHset_Pipelined` | GET on string keys | HSET on shared hash | P=10 |
| `BenchmarkTCP_Mixed_GetSet_Standard` | GET | SET | none |

Each benchmark uses `b.SetParallelism(13)` × `-cpu=4` ⇒ ~52 goroutines, deterministically split 50/50 reader/writer at startup via an atomic counter. All goroutines hit one server through individual TCP connections.

Profiles captured: CPU (`-cpuprofile`), allocations (`-memprofile -memprofilerate=4096`), block (`-blockprofile -blockprofilerate=10000`), mutex (`-mutexprofile -mutexprofilefraction=10`), and gctrace. Sampling rates match the existing `bench/profiles/run-profiles.sh` harness so cross-comparison stays apples-to-apples.

Capture command: `DUR=10s bash bench/profiles/run-profiles-mixed.sh`
Reports command: `OUT_LABEL=diagnosis-mixed bash bench/profiles/run-reports.sh`

## Throughput

| Workload | Iterations | Per-iter | Effective rps (commands) |
|---|---:|---:|---:|
| **Pipelined GetSet (mixed)** | 875 832 | 14 748 ns/op | **~425 k cmd/s** |
| **Pipelined GetHset (mixed)** | 724 131 | 17 174 ns/op | **~344 k cmd/s** |
| **Standard GetSet (mixed)** | 2 792 258 | 4 299 ns/op | **~107 k cmd/s** |

Reference: single-mode pipelined GET ~795 k rps and pipelined SET ~775 k rps on `main@bc563e6` (parent plan post-#27 figures). Mixed pipelined throughput is **~54%** of the per-mode sum (~785 k). Standard mixed (107 k) is **~58%** of the per-mode sum (~184 k). The cross-path interaction takes a measurable 40-something-percent bite, on top of the per-command engine cost.

## Where the contention lives

### Mutex profile — `Mixed_GetSet_Pipelined`

```
runtime.unlock (inline)              87.83%  (1234.77 ms / 1405.81 ms)
└── runtime.selectgo                 62.03%
    └── runtime.selunlock            62.01%
        ├── engine.sendAndWait       55.86%   ← channel-hop dispatch
        │   ├── HandleGet (via fast-path) 28.27%
        │   └── HandleSet (via fast-path) 27.60%
        └── …
sync.(*Mutex).Unlock                 12.15%   ← actual user mutexes
```

`runtime.unlock` here is the runtime-internal cache-line unlock used by select operations; `runtime.selectgo` reaches it through `runtime.selunlock`. The path from `engine.sendAndWait` to that point is **the channel-hop dispatch**: connection goroutine sends `Command{}` on `cmdChan`, parks on the result channel, engine goroutine receives and runs handler, sends result back, connection unparks. Two channel send/recv pairs per command, two scheduler hops.

### Mutex profile — `Mixed_GetHset_Pipelined`

```
runtime.unlock (partial-inline)      95.45%  (887.26 ms / 929.52 ms)
└── runtime.selectgo                 67.72%
    └── runtime.selunlock            67.11%
        └── engine.sendAndWait       53.25%
            ├── HandleGet            27.43%
            └── HandleHset           25.89%
sync.(*Mutex).Unlock                  4.49%   ← actual user mutexes
```

Even with collection writes on the writer side, the contention picture is the same: channel-hop dispatch dominates; user-level mutexes contribute under 5%.

### Mutex profile — `Mixed_GetSet_Standard`

```
runtime.unlock (inline)              99.36%
└── runtime.selectgo                 62.97%
    └── engine.sendAndWait           49.21%
        ├── HandleGet                ~25%
        └── HandleSet                ~25%
```

Same shape, slightly less concentrated because each command pays its own syscall + schedule round-trip (no batching).

### Block profile — `Mixed_GetSet_Pipelined`

```
runtime.selectgo                     67.50%  (70.22 s / 104.03 s)
├── server.handleConnection          58.75%
│   └── evaluator.evaluateFast       58.74%
│       └── command.Dispatch         58.71%
│           └── engine.sendAndWait   58.71%   ← channel hop wait
│               ├── HandleSet        29.70%
│               └── HandleGet        29.02%
runtime.chanrecv1                    19.81%   (testing harness park)
```

### Block profile — `Mixed_GetHset_Pipelined`

```
runtime.selectgo                     71.74%  (83.70 s / 116.67 s)
└── engine.sendAndWait               64.05%   ← channel hop wait
    ├── HandleHset                   31.93%
    └── HandleGet                    31.65%
```

Across both pipelined mixed workloads, the channel hop in `engine.sendAndWait` accounts for **58–64%** of total wait time. Reader and writer paths split the wait roughly 50/50, which is the expected outcome of a single global queue serving both paths.

### CPU profile — `Mixed_GetSet_Pipelined`

The CPU profile shows where time **runs**, not where it waits:

```
server.handleConnection              55.68% cum  ← all work descends from here
evaluator.Evaluate                   28.55%
runtime.syscall (Read+Write fds)     ~28%       ← TCP reads/writes
bufio.Writer.Flush                   19.05%
runtime.selectgo                     18.74%     ← cumulative time IN selectgo
```

`runtime.selectgo` consuming 18.74% of CPU (not just wait time) means the runtime is actively spinning in select arbitration on top of the wait time captured by the block profile. This is consistent with a hot channel-coordinated path: each command incurs one select on `cmdChan` (engine receive side) and one select on `resChan` (caller wait side).

## Per-design attribution

Issue #34's design A (per-shard locking) splits the global engine into N per-shard engines, each with its own `cmdChan`/`resChan` and its own `cache.shard.mu`. Mapping the diagnosis to design A:

| Cost source | Today | Under per-shard (N=16) |
|---|---|---|
| Channel-hop wait on `cmdChan`/`resChan` | One global queue serializes all 50 connections | Each shard serves ~3 connections on average; queue depth ↓ ~16× |
| `runtime.selectgo` arbitration | One target per goroutine; high contention | Distributed across N targets; should approach single-mode behavior |
| `cache.mu` mode-switching (the #28 risk) | N/A — write path bottleneck is the channel, not the lock | Each shard has its own mutex; readers and writers on different shards never share a lock |
| Cross-shard work (multi-key, MULTI/EXEC) | Free (everything serial) | Lock all touched shards in sorted order; bounded penalty |

Confidence the projection holds:
- **High** for single-key throughput (the dominant workload) — the bottleneck is the global channel and that's exactly what sharding partitions.
- **Medium** for mixed pipelined HSET — collection writes are still expensive per-op (slab allocation, packed encoding), so the absolute speedup is bounded by per-handler cost; the channel-hop relief still helps proportionally.
- **Medium-low** for workloads with > 30% multi-key ops (e.g. MGET-heavy applications) — sorted lock acquisition adds latency; needs prototype to confirm.

## Cross-path interaction probe

Mixed throughput vs sum of per-mode throughputs:

| Workload | Mixed | Sum of single-mode | Ratio |
|---|---:|---:|---:|
| Pipelined GetSet | 425 k cmd/s | 1 570 k (790 k SET + 790 k GET) | **27%** |
| Standard GetSet | 107 k cmd/s | ~184 k (~92 k SET + ~92 k GET) | **58%** |

Pipelined mixed retains only 27% of the additive single-mode throughput because the global cmdChan saturates at one engine's serial throughput. Standard mixed retains 58% because each command's syscall + schedule cost is large relative to the hop, so the relative penalty is smaller.

The 27% retention figure is a load-bearing diagnosis: any prototype must improve this materially. If a per-shard prototype with N=16 retains 80%+ of summed single-mode throughput on pipelined GetSet, design A is validated; less than ~50% retention would be a fail signal.

## What this pass did NOT confirm

- **No measurement of multi-key cross-shard cost.** The mixed benchmarks are all single-key (each command has one key argument). A subsequent prototype must include MGET / MSET probes to bound the cost of sorted-shard-ID lock acquisition.
- **No memory measurement.** Per-shard slabs replicate metadata × N; the +20% memory cap from #34 acceptance criteria is verified once an implementation exists, not now.
- **No production-realistic redis-benchmark numbers under mixed load.** Script `bench/redis-benchmark/run-mixed.sh` is shipped for prototype validation; this pass stuck to Go-level profiling because the question — "does the channel hop dominate?" — is best answered with profiles, not aggregate rps.

## Surprises (none of them disqualifying)

1. **`cache.mu` is essentially uncontended.** The single-engine architecture has a single acquirer (the engine goroutine), so contention only shows up in the channel queue, not in the lock itself. Per-shard locking redistributes work to N goroutines but each shard's mutex will still have a single dominant acquirer (its shard-engine goroutine), so we don't expect cache.mu to suddenly become contended. The risk is reversed from what one might initially imagine: per-shard locking does not need to "reduce lock contention" — it needs to "reduce queue contention."

2. **`runtime.selectgo` cumulative CPU is 18.7%, on top of its wait time.** Even ignoring the wait, select arbitration alone is consuming nearly a fifth of CPU. Per-shard locking makes selects shorter (fewer goroutines per channel) and reduces arbitration cost in addition to wait cost.

3. **Mixed-vs-single-mode ratio is workload-dependent.** Standard mixed retains 58% of summed throughput vs pipelined mixed at 27%. Per-shard locking should equalize these ratios by removing the queue as the shared resource.

## Risks identified for prototype

| Risk | Magnitude bound | Mitigation in prototype |
|---|---|---|
| Multi-key sorted-acquire latency overshoots single-key gain | Worst case: KEYS visits 16 shards × ~3 µs RLock = ~50 µs | Prototype includes MGET-heavy probe |
| Per-shard slab metadata × N exceeds memory budget | N × per-allocator overhead, expected ~few MB total at N=16 | Prototype measures; if exceeded, switch to thread-safe slab |
| N too small ⇒ residual queue contention; N too large ⇒ wasted shards | Bounded by N-sweep over {4, 8, 16, 32} | Prototype acceptance includes the optimal-N decision |
| cache.shard.mu becomes contended under per-shard if multiple workers exist per shard | Should not — each shard has 1 engine goroutine | Prototype mutex profile; abort if cache.shard.mu > 30% of contention |

## Conclusion

Channel-hop dispatch dominates mixed-workload contention with high confidence. Per-shard locking targets the right cost. Prototype work (single-key path + N-sweep) is unblocked.

**Next step:** branch off `diag/per-shard-locking` after this PR merges; build a per-shard prototype gated behind a `shardproto` build tag; run the same three mixed benchmarks against the prototype and compare the channel-hop wait-time figures.

## Files

```
bench/results/diag-per-shard-locking/
├── diagnosis-summary.md                  (this file)
└── profiles/diagnosis-mixed/
    ├── cpu/{tcp-mixed-getset-pipe,tcp-mixed-gethset-pipe,tcp-mixed-getset-std}.{prof,top.txt}
    ├── alloc/<same labels>.{mem,objects.txt,space.txt}
    ├── block/<same labels>.{prof,top.txt}
    ├── mutex/<same labels>.{prof,top.txt}
    ├── gctrace/<same labels>.gctrace
    ├── trace/{tcp-mixed-getset-pipe,tcp-mixed-gethset-pipe}.trace   (large; gitignored)
    └── <label>.bench.txt                (per-workload bench output)
```

## Reproduce

```bash
# Profile capture (~1 min total)
DUR=10s bash bench/profiles/run-profiles-mixed.sh

# Top-30 reports
OUT_LABEL=diagnosis-mixed bash bench/profiles/run-reports.sh

# Optional: redis-benchmark concurrent SET+GET (requires Docker)
bash bench/redis-benchmark/run-mixed.sh diag
```
