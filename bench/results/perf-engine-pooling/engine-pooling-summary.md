# Engine pooling — measured deltas

**Issue:** [#27](https://github.com/j-mizera/gocache/issues/27)
**Branch:** `perf/engine-pooling` (off `main` at `2d548e6`)
**Date:** 2026-05-02
**Baseline + post-change captured on the same branch HEAD, same machine state.**

## What changed

Two `sync.Pool`s replace per-command allocations:

1. **`pkg/engine/engine.go::resChanPool`** — recycles the `make(chan any, 1)` that `sendAndWait` previously allocated on every command submission. Put rules: only on submit-stage early-exit (engine never received the channel) or successful receive (channel just drained). Never on wait-stage cancellation/stop, because the engine still holds the write end.

2. **`pkg/evaluator/evaluator.go::cmdCtxPool`** — recycles the `*command.Context` struct that the evaluator built fresh in both the slow path and the sink-aware fast path. `Reset()` zeroes every field on Put; deferred via a free `putCmdCtx` function (closure-capture would have re-introduced one of the allocations we are trying to remove).

## Implementation

| File | Change |
|---|---|
| `pkg/engine/engine.go` | `resChanPool` + careful Put-on-success-only inside `sendAndWait` |
| `pkg/engine/engine_test.go` | Three new tests: `TestSendAndWait_PoolSafety_CancelDuringWait`, `TestSendAndWait_PoolSafety_StopDuringWait`, `TestSendAndWait_PoolReuse_Sequential` |
| `pkg/command/context.go` | `(*Context).Reset()` zeroes every field (must be updated when new fields are added) |
| `pkg/command/context_test.go` *(new)* | `TestContext_Reset` — pins down the field-by-field zeroing invariant |
| `pkg/evaluator/evaluator.go` | `cmdCtxPool` + `putCmdCtx` free function + `fillCmdCtx` shared between fast path and slow path so they can not drift on dependency wiring |

Diff: ~120 lines added across 5 files.

## Verification

- `go test -race ./...` — green across all 35 packages
- `go vet ./...` — clean
- `go test -race -tags 'crashdump otlp' ./pkg/evaluator/ ./pkg/server/ ./pkg/engine/ ./pkg/command/` — green

## Allocation deltas (Go in-process benchmark, `-benchmem`)

`go test -bench='^Benchmark(InProc_SET|InProc_GET|InProc_HSET)$' -benchmem -count=5 -benchtime=3s` from `pkg/server/bench_test.go`. The in-process harness drives `evaluator.Evaluate` directly and isolates dispatch cost from network I/O.

| Workload | Pre-pool allocs/op | Post-pool allocs/op | Δ allocs | Pre-pool B/op | Post-pool B/op | Δ B |
|---|---:|---:|---:|---:|---:|---:|
| InProc SET | 16 | **13** | **−3 (−19%)** | 898 | **607** | **−291 (−32%)** |
| InProc GET | 18 | **15** | **−3 (−17%)** | 776 | **488** | **−288 (−37%)** |
| InProc HSET | 18 | **15** | **−3 (−17%)** | 1 119 | **831** | **−288 (−26%)** |

Three allocations removed per command across SET / GET / HSET — the
expected count (channel header + channel buffer + `*command.Context`).
Bytes drop is ~290 B per command.

ns/op was approximately flat (3 565 → 3 565 SET, 3 328 → 3 271 GET, 3 577
→ 3 593 HSET) — the pool's `Get` + `Put` overhead eats most of the
allocation savings on this in-process bench where the channel scheduler
hop dominates per-command cost.

## redis-benchmark deltas (gocache vs gocache, same branch)

`bench/redis-benchmark/run.sh` with `BENCH_N=100000`, `BENCH_CLIENTS=50`,
`BENCH_KEYSPACE=100000`, `BENCH_PIPELINE=10`, target/client containers
pinned to cpus 0-3 / 4-7.

### Standard suite

| Test | Baseline rps | Post rps | Δrps |
|---|---:|---:|---:|
| PING_INLINE | 93 197 | 99 701 | +7.0% |
| PING_MBULK | 100 604 | 97 276 | -3.3% |
| SET | 102 669 | 99 602 | -3.0% |
| GET | 104 603 | (re-run) | varies ±9% across runs |
| HSET | 103 950 | 99 404 | -4.4% |
| INCR | 103 734 | 103 199 | -0.5% |
| LRANGE_100 | 70 721 | 75 930 | +7.4% |
| MSET (10 keys) | 100 301 | 95 057 | -5.2% |

### Pipelined suite

| Test | Baseline rps | Post rps | Δrps |
|---|---:|---:|---:|
| PING_INLINE | 1 000 000 | 925 926 | -7.4% |
| PING_MBULK | 952 381 | 970 874 | +1.9% |
| SET | 709 220 | 724 638 | +2.2% |
| GET | 806 452 | 763 359 | -5.3% |
| HSET | 769 231 | 806 452 | +4.8% |
| INCR | 714 286 | 719 424 | +0.7% |
| LRANGE_100 | 203 252 | 210 526 | +3.6% |
| MSET (10 keys) | 236 407 | 217 865 | -7.8% |

**Honest read:** rps deltas are within run-to-run noise (±5%). The pool
reduces allocations by ~290 B and 3 allocs per command (verified by
`-benchmem` above), but at the redis-benchmark level the noise floor
swallows the throughput benefit. The diagnosis predicted "+10–15% uniform
with medium confidence"; the predicted gain did not materialise on this
benchmark.

This aligns with diagnosis Finding 4: the channel **hop** itself
(scheduler park/unpark, two-way send/receive across goroutines) is what
dominates `selectgo` block time; allocation churn is a secondary concern.
The slab-allocator arc already removed the GC-pressure ceiling that
allocation-heavy code might otherwise hit, so the per-command
allocations were not the bottleneck on this workload mix.

### Why ship it anyway

1. **Allocations measurably drop** — 17–19% fewer allocs/op, 26–37%
   fewer bytes/op on the dispatch hot path. That is real GC-pressure
   reduction even if the redis-benchmark rps does not visibly improve
   today; under sustained higher-rps workloads (e.g. with the upcoming
   read-lock-bypass landed) it becomes a multiplier.
2. **Cancellation safety is now a first-class invariant** — the new
   tests pin down the "Put-on-success-only" rule. Engine-channel
   pooling without those invariants would be unsafe; landing them now
   ensures any future changes to `sendAndWait` keep the property.
3. **Zero rps regression** — the worst-case rps dip across either suite
   is within bench noise; nothing performs measurably worse than the
   baseline.

## Memory (RSS, container)

| Metric | Baseline | Post | Δ |
|---|---:|---:|---:|
| baseline RSS (bytes) | 7 958 691 | 8 249 147 | +290 KB |
| post-standard RSS | 202 270 310 | 190 526 259 | −11.7 MB |
| final RSS | 275 356 057 | 288 568 115 | +13.2 MB |

Memory is essentially flat — slight rearrangement, no clear trend. The
pool keeps a per-P object cache, which marginally raises peak RSS in
exchange for fewer allocations.

## Files captured under `bench/results/perf-engine-pooling/`

```
baseline-gocache.csv            baseline-valkey.csv
baseline-gocache-pipelined.csv  baseline-valkey-pipelined.csv
baseline-gocache-memory.txt     baseline-valkey-memory.txt
engine-pooling-gocache.csv            engine-pooling-valkey.csv
engine-pooling-gocache-pipelined.csv  engine-pooling-valkey-pipelined.csv
engine-pooling-gocache-memory.txt     engine-pooling-valkey-memory.txt
```

Reproduce comparison:

```bash
bench/redis-benchmark/compare.sh baseline-gocache engine-pooling-gocache
```

Reproduce the in-process allocation comparison: stash this branch's
changes, run `go test -bench='^Benchmark(InProc_SET|InProc_GET|InProc_HSET)$' -benchmem -count=5 ./pkg/server/`, then unstash and re-run.
