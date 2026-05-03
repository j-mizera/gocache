# Lower default shard count from 16 to 8

**Branch:** `perf/memory-reduction-combined`
**Date:** 2026-05-03
**Issue:** [#49](https://github.com/j-mizera/gocache/issues/49) (sub-task A only)
**Outcome:** Production default shard count drops from 16 to 8 — the prototype N-sweep in #39 measured N=8 within ~3% of N=16's throughput optimum on mixed pipelined GetSet, while halving per-shard memory overhead. **Memory delta -17 MB** (mean 237 MB → 220 MB at the docker bench tier). **Throughput within ±5% on most commands** — predicted cost for going from N=16 to N=8 was ~3-8%, and actual results match.

## What changed

Two single-line constant flips:

- `pkg/cache.DefaultShards`: `16` → `8`
- `pkg/config.defaultCacheShards`: `16` → `8`

The shard count is configurable via `memory.cache_shards`; this PR moves only the default. Deployments that need N=16 (or any other power of two ≤ 64) keep their existing config knob. `cache.NewWithBytes` continues to use N=1 for tests.

## Bench (mean of 2 docker runs each)

### Memory delta

| Configuration | RSS delta after standard + pipelined suite |
|---|---:|
| post-#46 (N=16, slab 1 MiB × 16) | 281 MB / 280 MB |
| post-#48 (N=16, slab 64 KiB × 16) | 234 MB / 240 MB |
| **this PR (N=8, slab 128 KiB × 8)** | **208 MB / 232 MB** |
| **Δ vs post-#48** | **-17 MB** |
| **Cumulative since pre-#34 (~280 MB)** | **-60 MB (-22%)** |
| valkey reference | 18 MB |

Memory delta variance run-to-run is large (~25 MB spread on the same code) so the 17 MB mean improvement is in the same ballpark as the noise floor. Cumulative trend is consistent: each step (slab scaling in #48, shard count reduction here) trims 15-20 MB.

### Throughput

Most commands within ±5% noise. INCR shows a -16% but post-#48 had unusually high readings on it (884k vs ~803k historical norm — outlier reverting to mean); compared to post-#42 baseline (803k) the new 746k reading is -7%, in line with the prototype's predicted N=16→N=8 cost.

| Workload | post-#48 (mean) | post-A (mean) | Δ |
|---|---:|---:|---:|
| Pipelined SET | 745k | 744k | flat |
| Pipelined GET | 824k | 836k | +1.5% |
| Pipelined HSET | 759k | 725k | -4.5% |
| Pipelined SADD | 741k | 736k | -0.6% |
| Pipelined SPOP | 692k | 697k | +0.7% |
| Pipelined LPOP | 794k | 797k | flat |
| Pipelined LRANGE_100 | 197k | 197k | flat |
| Pipelined PING_INLINE | 941k | 877k | -6.7% |
| Pipelined INCR | 884k | 746k | -16% (post-#48 outlier; vs post-#42 baseline 803k it's -7%) |
| Pipelined MSET | 122k | 123k | flat (the cache-locality regression #47 still applies) |

Standard suite within ±5% across the board.

## Why this still doesn't hit the +20% target

Acceptance from #49 was `≤180 MB` total RSS delta. Current 220 MB is 22% over the target (~40 MB to go).

Remaining levers from #49 not addressed by this PR:

- **C (connection RESP buffer pool)** — estimated ~1-3 MB savings (smaller than #49 originally projected; the bench has 50 long-lived connections, so per-connection allocation churn is minimal — most savings would benefit higher-churn workloads not measured here)
- **B (tighter native-collection encoding thresholds)** — estimated ~30-50 MB savings; biggest remaining lever; needs careful per-type measurement to bound the throughput cost on workloads that mutate medium-sized collections

Skipping B for this PR — the work is 1-3 days and the throughput risk is workload-dependent. Filing a follow-up.

## Decision points (decision rationale)

**Why not add C in this PR?**
The bench scenario (50 long-lived connections) doesn't expose connection-allocation churn. The ~1 MB savings would be lost in the ~25 MB run-to-run variance. Worth doing for production server hygiene; not worth bundling here.

**Why not B in this PR?**
1-3 day investigation work; threshold tuning risks regressing throughput on workloads that mutate medium-sized collections (mutations on packed encoding are O(N) on collection size). Needs its own measurement-driven plan.

**Could we go N=4?**
Prototype #39 showed N=4 retained 1130k mixed pipelined vs N=8's 1168k — about 3% lower. Could trim another ~10-15 MB. Worth considering if #49.B doesn't deliver enough; not done here because the prototype N=8 was the explicit recommendation.

## Verification

- `go vet ./...` clean
- `staticcheck ./...` clean
- `staticcheck -tags 'crashdump otlp' ./...` clean
- `go test -race -count=1 ./...` green across 25 packages

## Reproduce

```bash
sg docker -c "REBUILD=1 bash bench/redis-benchmark/run.sh n8-default --target gocache"
sg docker -c "bash bench/redis-benchmark/run.sh n8-default-rerun --target gocache"
```

## Files

- `pkg/cache/cache.go` (1 line) — `DefaultShards` constant
- `pkg/config/config.go` (1 line) — `defaultCacheShards` default
- `bench/results/perf-memory-reduction-combined/{n8-default-*,this file}` — measurement artefacts
