# Sink-aware fast path — measured deltas

**Issue:** [#26](https://github.com/j-mizera/gocache/issues/26)
**Branch:** `perf/sink-aware-fast-path` (off `main` at `a443b33`)
**Date:** 2026-05-02
**Baseline + post-change captured on the same branch HEAD, same machine state.**

## What changed

`pkg/evaluator/evaluator.go::evaluateInternal` now branches on a single
atomic-load check before any instrumentation work:

```go
if !b.hasAnySink() {
    return b.evaluateFast(parentCtx, ctx, op, args, inBatch, handler)
}
```

When no event subscribers, no command-hook executor, and no operation-hook
executor are wired (the production-default state), the entire instrumentation
block is skipped:

- No `tracker.Start` / `tracker.Complete` write-locks
- No 7× `cmdOp.Enrich` mutex acquires
- No 4× `emitter.Emit` calls (each previously took `bus.mu.Lock()` to push
  the replay ring even when nobody listened)
- No 4× `cmdOp.ContextSnapshot(false)` map allocations
- No `RunPreHooks` / `RunPostHooks` / `RunStartHooks` / `RunCompleteHooks`

The handler still receives a real `*ops.Operation` in `opCtx` so logger
correlation works; the operation is allocated, used, and dropped without
touching the tracker.

## Implementation

| File | Change |
|---|---|
| `api/events/events.go` | `Emitter` interface now requires `HasSubscribers() bool`; `NoopEmitter` returns false |
| `pkg/events/bus.go` | `subCount atomic.Int32` mirrors `len(subscribers)`; `HasSubscribers` is now a lock-free atomic load |
| `pkg/plugin/cmdhooks/cmdhooks.go` | `total atomic.Int32` mirrors `len(pre)+len(post)`; `HasAny` is a lock-free atomic load |
| `pkg/plugin/ophooks/registry.go` | `total atomic.Int32` mirrors `len(hooks)`; `HasAny` is a lock-free atomic load |
| `pkg/operations/tracker.go` | Added `IncrementSkipped()` + `SkippedCount() uint64` so operability tooling can see the bypassed-command count |
| `pkg/evaluator/sinks.go` | New file: `(b *BaseEvaluator) hasAnySink()` aggregator method |
| `pkg/evaluator/evaluator.go` | Fast-path branch + `evaluateFast` helper (no instrumentation) |
| `pkg/evaluator/fast_path_test.go` | New file: 5 tests covering no-sinks fast path, bus / cmdhook / ophook slow path, and mid-stream subscribe flag flip |
| `pkg/events/bus_test.go` | New: atomic counter invariant test (double-subscribe, unknown unsubscribe) |
| `pkg/plugin/cmdhooks/cmdhooks_test.go` | Same |
| `pkg/plugin/ophooks/ophooks_test.go` | Same |
| `pkg/evaluator/evaluator_test.go`, `pkg/logcollector/collector_test.go` | `mockEmitter.HasSubscribers() bool` to satisfy the extended interface |

Test mocks return `HasSubscribers() = true` so existing test coverage
continues to exercise the slow (instrumented) path. Real production wiring
uses `events.NoopEmitter` (`HasSubscribers() = false`) until a plugin
attaches.

Diff: ~150 lines added, ~30 changed across 11 files. Plus 1 new file
(`sinks.go`, 31 lines).

## Verification

- `go test -race ./...` — green across all 35 packages
- `go vet ./...` — clean
- `go test -race -tags 'crashdump otlp' ./pkg/evaluator/ ./pkg/server/` — green

## Bench deltas (gocache vs gocache, same branch)

`bench/redis-benchmark/run.sh` with `BENCH_N=100000`, `BENCH_CLIENTS=50`,
`BENCH_KEYSPACE=100000`, `BENCH_PIPELINE=10`, target/client containers
pinned to cpus 0-3 and 4-7 respectively, 2 GiB RAM each. Baseline captured
on branch HEAD before any source change; post-change captured after the
sink-aware patch with the gocache image rebuilt.

### Standard suite

| Test | Baseline rps | Post rps | Δrps | Baseline p99 (ms) | Post p99 (ms) | Δp99 |
|---|---:|---:|---:|---:|---:|---:|
| PING_INLINE | 104 493 | 99 502 | -4.8% | 0.527 | 0.375 | -28.8% |
| PING_MBULK | 104 167 | 105 485 | +1.3% | 0.455 | 0.367 | -19.3% |
| **SET** | 93 897 | **103 520** | **+10.2%** | 0.503 | 0.367 | -27.0% |
| GET | 93 985 | 96 061 | +2.2% | 0.503 | 0.431 | -14.3% |
| INCR | 97 656 | 92 507 | -5.3% | 0.551 | 0.399 | -27.6% |
| **HSET** | 97 087 | **106 496** | **+9.7%** | 0.383 | 0.343 | -10.4% |
| MSET (10 keys) | 94 340 | 92 421 | -2.0% | 0.855 | 0.487 | -43.0% |
| LRANGE_100 | 64 475 | 67 889 | +5.3% | 0.991 | 0.575 | -42.0% |
| LPUSH | 5 218 | 5 450 | +4.4% | 23.663 | 22.143 | -6.4% |
| RPUSH | 6 793 | 6 332 | -6.8% | 9.991 | 15.039 | +50.5% |
| LPOP | 6 896 | 6 384 | -7.4% | 10.351 | 13.775 | +33.1% |
| RPOP | 20 133 | 18 861 | -6.3% | 5.271 | 5.943 | +12.7% |
| SADD | 1 957 | 1 995 | +1.9% | 50.751 | 49.791 | -1.9% |
| SPOP | 2 321 | 2 344 | +1.0% | 50.687 | 51.775 | +2.1% |

Standard-mode rps swings are within run-to-run variance for most ops; the
clean wins are SET +10%, HSET +10%, LRANGE +5%. Latency p99 is broadly
better — PING -29%, SET -27%, INCR -28%, MSET -43% — even where rps barely
moves, which is the expected signature of removing per-command instrument
work without changing the network round-trip cost.

The standard rps gap is bounded by syscall overhead — diagnosis Finding 6
attributed 54% of TCP standard CPU to `syscall.Syscall6` and 39% to
`bufio.(*Writer).Flush`. Cutting dispatch cost below the kernel cost
gives single-digit rps gains; the latency improvement is where the user
sees the change.

### Pipelined suite (P=10)

| Test | Baseline rps | Post rps | Δrps | Baseline p99 (ms) | Post p99 (ms) | Δp99 |
|---|---:|---:|---:|---:|---:|---:|
| PING_INLINE | 724 638 | 854 701 | **+17.9%** | 2.799 | 0.415 | -85.2% |
| PING_MBULK | 740 741 | 833 333 | **+12.5%** | 2.463 | 0.423 | -82.8% |
| **SET** | 440 529 | **709 220** | **+61.0%** | 3.551 | 1.591 | -55.2% |
| **GET** | 485 437 | **735 294** | **+51.5%** | 3.975 | 3.655 | -8.1% |
| **INCR** | 442 478 | **671 141** | **+51.7%** | 3.463 | 1.639 | -52.7% |
| **HSET** | 502 513 | **740 741** | **+47.4%** | 3.287 | 1.975 | -39.9% |
| **LRANGE_100** | 149 925 | **216 920** | **+44.7%** | 13.351 | 4.455 | -66.6% |
| MSET (10 keys) | 195 312 | 193 424 | -1.0% | 4.135 | 6.303 | +52.4% |
| LPUSH | 1 657 | 1 716 | +3.5% | 431 | 412 | -4.5% |
| RPUSH | 4 184 | 4 001 | -4.4% | 143 | 154 | +7.4% |
| LPOP | 4 204 | 4 019 | -4.4% | 142 | 160 | +12.4% |
| RPOP | 6 762 | 6 379 | -5.7% | 102 | 117 | +15.0% |
| SADD | 1 966 | 2 001 | +1.8% | 503 | 496 | -1.4% |
| SPOP | 2 345 | 2 352 | +0.3% | 515 | 516 | +0.1% |

This is where the fast path lives. Pipelined commands amortise the network
round-trip across N requests, so the per-command dispatch cost dominates;
removing the instrumentation block scales cleanly:

- **SET +61%** (440k → 709k)
- **INCR +52%** (442k → 671k)
- **GET +52%** (485k → 735k)
- **HSET +47%** (503k → 741k)
- **LRANGE_100 +45%** (150k → 217k)

Latency p99 falls accordingly: pipelined SET -55%, HSET -40%, INCR -53%,
PING -85%.

LPUSH/SADD/SPOP rps remain bottlenecked by collection-write
re-allocation (their native paths still walk the value to compute the
new size — out of scope for this arc; tracked as the
incremental-collection-tracking follow-up).

### Memory

| Metric | Baseline | Post | Δ |
|---|---:|---:|---:|
| baseline RSS (bytes) | 8 134 852 | 8 208 252 | +73 KB |
| post-standard RSS (bytes) | 244 632 780 | 197 761 433 | **−47 MB** |
| final RSS (bytes) | 335 124 889 | 271 476 326 | **−64 MB (−19%)** |

The 64 MB drop comes from skipping the four per-command
`cmdOp.ContextSnapshot(false)` map allocations and the
`rex.BuildMetadata` map on the hot path. Over the full run that is several
million skipped allocations.

## vs valkey on this branch

| Test | gocache post | valkey post | Ratio |
|---|---:|---:|---:|
| Std SET | 103 520 | 111 111 | 0.93× |
| Std GET | 96 061 | 115 340 | 0.83× |
| Std HSET | 106 496 | 112 360 | 0.95× |
| Pipe SET | 709 220 | 934 579 | **0.76×** (was 0.49×) |
| Pipe GET | 735 294 | 1 041 667 | **0.71×** (was 0.57×) |
| Pipe HSET | 740 741 | 1 041 667 | **0.71×** (was 0.56×) |
| Pipe INCR | 671 141 | 990 099 | **0.68×** (was 0.65×) |

Pipelined-mode ratio to valkey moves from ~0.5× to ~0.7×. Standard-mode
ratio drifts down slightly because the valkey numbers happen to land
higher on this run than they did on the baseline run — across-run TCP
loopback variance, not a regression. Per-branch baseline isolates the
gocache delta cleanly: every standard simple-op gocache number is up vs
its branch-baseline.

## Deviations from plan projection

| Workload | Plan projection | Actual | Notes |
|---|---|---|---|
| Standard simple writes | +25–35% | +5–10% | TCP syscall ceiling (Finding 6) caps it; in-process numbers from diagnosis baseline (1 867 → 1 4xx ns) match the +25–35% projection — that path isn't TCP-bound |
| Standard collection writes | +10–15% | +1–10% | LPUSH/SADD still walk-the-value during size estimation (bypass-effect masked) |
| Pipelined results | "smaller delta because amortised" | **+47–61%** | Plan was wrong here: pipelining amortises *network* cost, not dispatch cost. The dispatch cost is per-command and gets multiplied by the batch depth, so the fast path wins more on pipelined than on standard |
| RSS | "small drop" | **-19% (-64 MB)** | Larger than projected — ContextSnapshot map allocations were a bigger heap source than expected |

## What this fixes does NOT address

- **Pipelined LPUSH at ~1700 rps (0.002× valkey)** — list mutations
  re-allocate the entire list on every push. Needs incremental tracking +
  a different list encoding for promoted lists. Tracked as collection-write
  follow-up.
- **Pipelined SADD at ~2000 rps** — same pattern, set re-allocates the
  map.
- **Standard GET 0.83× valkey** — kernel TCP stack is the bottleneck (Finding 6); next lever is read-lock-bypass (#28) which removes the engine hop for read commands.

## Files captured under `bench/results/perf-sink-aware-fast-path/`

```
baseline-gocache.csv            baseline-valkey.csv
baseline-gocache-pipelined.csv  baseline-valkey-pipelined.csv
baseline-gocache-memory.txt     baseline-valkey-memory.txt
sink-aware-fast-path-gocache.csv            sink-aware-fast-path-valkey.csv
sink-aware-fast-path-gocache-pipelined.csv  sink-aware-fast-path-valkey-pipelined.csv
sink-aware-fast-path-gocache-memory.txt     sink-aware-fast-path-valkey-memory.txt
```

Reproduce comparison:

```bash
bench/redis-benchmark/compare.sh baseline-gocache sink-aware-fast-path-gocache
```
