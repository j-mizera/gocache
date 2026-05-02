# Read-lock bypass — measurement-driven negative result + banked cleanups

**Issue:** [#28](https://github.com/j-mizera/gocache/issues/28)
**Branch:** `perf/read-lock-bypass` (off `main` at `ffbd6fd`)
**Date:** 2026-05-02
**Outcome:** the engine bypass for read-only commands did NOT improve
redis-benchmark throughput; it regressed pipelined writes by roughly the
same amount it gained on reads, because `sync.RWMutex` mode-switching
costs both ways. The bypass branch was reverted. The supporting
infrastructure — `Spec.ReadOnly` classification, atomic `LastAccessNs`,
sampled-LRU eviction, and the `WatchDirty` race fix surfaced by the new
WATCH stress test — stays.

## What was tried

`pkg/evaluator/evaluator.go::evaluateInternal` was modified to, for
commands marked `Spec.ReadOnly`, take `cache.RLock()` and run the
handler inline (set `inBatch = true` so `command.Dispatch` skips the
engine queue). Mutations kept going through the engine.

The full bypass implementation:

- `Spec.ReadOnly` field on every read-only handler (GET, MGET, EXISTS, HGET,
  HGETALL, HKEYS, HVALS, HLEN, LRANGE, LLEN, SMEMBERS, SISMEMBER, SCARD,
  SINTER, SUNION, SDIFF, ZSCORE, ZCARD, ZRANGE, ZRANK, ZCOUNT, KEYS, SCAN,
  RANDOMKEY, TTL, PTTL, TYPE, OBJECT, STRLEN, DBSIZE, INFO, PING, ECHO).
  Read-like commands with side effects (GETSET, GETDEL, BLPOP, BRPOP, LPOP,
  RPOP, SPOP) explicitly NOT marked.
- `Cache.RawGet` now updates `LastAccessNs` via `atomic.StoreInt64` and no
  longer calls `lruMoveToFront`. The list mutation requires exclusive
  access; under `RLock` it would race.
- `evictLRU` switched to Redis-style sampled approximate LRU
  (`maxmemory-samples = 8`) — samples 8 entries from the LRU tail, evicts
  the one with the oldest `LastAccessNs`. Preserves the existing
  "read-recently means survives eviction" semantic even though the linked
  list is now write-ordered only.
- `ClientContext.WatchDirty` → `atomic.Bool`. This fixed a pre-existing
  race surfaced by the new `TestIT_WatchPropagation_ReadLockBypass`
  stress test: `NotifyMutation` (called from the engine goroutine under
  `cache.Lock`) wrote `WatchDirty = true` while `HandleExec` (on the
  connection goroutine, no cache lock) read it. The race existed in main
  but no test exercised it.

## What was measured

`bench/redis-benchmark/run.sh` with `BENCH_N=100000`, `BENCH_CLIENTS=50`,
`BENCH_KEYSPACE=100000`, `BENCH_PIPELINE=10`, target/client containers
pinned to cpus 0-3 / 4-7. Two post-bypass samples averaged vs the
pre-bypass baseline:

| Workload | Baseline rps | Post-bypass avg rps | Δ |
|---|---:|---:|---:|
| Pipelined SET | 775 194 | 692 951 | **-10.6%** |
| Pipelined PING_INLINE | 990 099 | 885 580 | **-10.6%** |
| Pipelined HSET | 819 672 | 775 381 | -5.4% |
| Pipelined GET | 793 651 | 820 168 | **+3.3%** |
| Pipelined INCR | 719 424 | 730 277 | +1.5% |
| Standard SET | 90 662 | 91 410 | +0.8% |
| Standard GET | 92 937 | 90 525 | -2.6% |
| Standard HSET | 111 235 | 102 967 | -7.4% |
| Standard PING_INLINE | 106 952 | 89 731 | -16.1% |

Run-to-run variance on the SAME post-bypass build is ±15% on pipelined
SET (645k vs 740k between consecutive runs), so the picture in honest
terms:

- **Pipelined GET: marginal gain (~+3%)** — within noise
- **Pipelined SET / PING / HSET: small-to-moderate regression** —
  partially outside noise
- **Plan's projected "+45% pipelined GET → 0.85× valkey": not
  observed.** Pipelined GET stayed ~0.83× valkey, the same ratio
  delivered by the previously-shipped sink-aware fast path.

## Why the bypass did not help

The diagnosis (Finding 4) correctly identified that 42% of pipelined-GET
wait time was `runtime.selectgo` reached via `engine.sendAndWait` — the
channel hop. The bypass removes that hop. But the bypass also introduces
a second cost: every command that reaches the cache, read or write,
now interacts with `sync.RWMutex` mode-switching.

Before the bypass: only the engine goroutine ever touched
`cache.RWMutex`, and only via `Lock()`. Mode never switched. Cache-line
state on the RWMutex was minimal.

After the bypass: connection goroutines call `RLock`/`RUnlock`; the
engine still calls `Lock`/`Unlock`. Each `Lock` now does
`atomic.AddInt32(&readerCount, -rwmutexMaxReaders)` (writer-pending
signal) plus a wait for in-flight readers via `readerWait`. Even with no
readers actually present, the atomic-RMW dance on the shared cache line
costs ~30 ns, multiplied by 700 k+ ops/sec on pipelined writes.

Standard SET (single client) showed ~0% change because there are no
concurrent readers to switch with. Pipelined SET (50 clients × 10
in-flight) showed -11% because the engine's tight `Lock/Unlock` loop
runs in the contended state where atomic-RMW costs add up.

## What this PR ships (banked wins)

Reverted the bypass branch in `evaluateInternal` (5 lines). Kept
everything else, on the basis that each piece is independently
defensible:

| Change | Standalone value |
|---|---|
| `api/command.Spec.ReadOnly bool` + handler classifications | Documents the intent for any future routing layer (per-shard locks, lock-free reads, read-replica routing). Zero runtime cost. |
| `Cache.RawGet` atomic `LastAccessNs` + no list mutation | Without the bypass, `RawGet` is only called under `cache.Lock` so the atomic is unnecessary — but it's also harmless. The write-path `lruPushFront` / `lruMoveToFront` remain authoritative. |
| Sampled-LRU eviction | Improves eviction algorithm to Redis-style `maxmemory-samples=8`. Independent correctness improvement. Preserves the test-pinned semantic that recently-read keys survive eviction. |
| `ClientContext.WatchDirty` → `atomic.Bool` | **Pre-existing race fix.** `NotifyMutation` writes from one goroutine under `cache.Lock`; `HandleExec` reads from a different goroutine without that lock. Fixed independently of bypass. |
| `TestIT_WatchPropagation_ReadLockBypass` stress test | 500 ms concurrent SET-loop + WATCH/MULTI/GET/EXEC-loop on the same key. Verifies WATCH dirty-bit propagation is race-free. Surfaced the pre-existing race fixed above. |
| `TestSpec_ReadOnly_Classification` | Table-driven test enumerating every command and asserting its `ReadOnly` bit. Catches drift when new handlers are added. |

## After-revert verification

`bench/redis-benchmark/run.sh cleanups-only --target gocache` after
reverting the bypass branch:

| Workload | Baseline rps | Cleanups-only rps | Δ |
|---|---:|---:|---:|
| Pipelined SET | 775 194 | 746 269 | -3.7% (within noise) |
| Pipelined GET | 793 651 | 769 231 | -3.1% (within noise) |
| Pipelined HSET | 819 672 | 800 000 | -2.4% (within noise) |
| Pipelined PING_INLINE | 990 099 | 1 086 956 | +9.8% (within noise) |
| Standard SET | 90 662 | 95 694 | +5.6% (within noise) |
| Standard GET | 92 937 | 84 746 | -8.8% (edge of noise) |

All deltas inside the ±10% run-to-run variance floor. The mode-switching
regression on pipelined writes is gone.

## Methodological lesson — captured for future plans

The diagnosis pass profiled each workload in isolation: pipelined GET on
its own, pipelined HSET on its own, etc. That correctly attributed the
channel-hop cost on each path. It did NOT capture the cross-workload
interaction of changing one path's synchronization on another path's
synchronization — when the bypass was added, the mode-switching cost
showed up on workloads the diagnosis hadn't profiled.

Action items, applied to the parent plan and the diagnosis-pass plan:

1. Mixed-workload step added to the diagnosis ritual: any plan that
   changes a synchronization primitive (channel, lock, mutex pattern)
   must run a 50%-read 50%-write interleaved benchmark
   (`valkey-benchmark -t set,get` etc.) alongside the per-command
   profiles. Cheap to add, would have caught this before
   implementation.

2. Block profiles attribute single-workload wait time. They don't
   reflect ecosystem cost. Where two paths share state (here:
   `sync.RWMutex`), confirming "removing the cost on path A doesn't add
   cost to path B" is part of the diagnosis, not an implementation
   afterthought.

3. The plan's risk table for #28 listed: "RLock contention spikes when
   both read and write loads are high — acceptable; if contention
   becomes an issue, parent plan documents per-shard locks are out of
   scope." The risk was anticipated. The magnitude (regression > gain)
   was not. "Acceptable" should require a measured ceiling, not just
   acknowledgement.

These updates land in `projects/gocache/plans/command-flow/diagnosis-pre-implementation.md`
and `projects/gocache/plans/command-flow-optimization.md` (Obsidian).

## Files captured under `bench/results/perf-read-lock-bypass/`

```
baseline-gocache.csv                  baseline-valkey.csv
baseline-gocache-pipelined.csv        baseline-valkey-pipelined.csv
baseline-gocache-memory.txt           baseline-valkey-memory.txt
read-lock-bypass-gocache.csv          read-lock-bypass-valkey.csv
read-lock-bypass-gocache-pipelined.csv  read-lock-bypass-valkey-pipelined.csv
read-lock-bypass-gocache-memory.txt   read-lock-bypass-valkey-memory.txt
read-lock-bypass-rerun-gocache.csv    (second sample of post-bypass)
read-lock-bypass-rerun-gocache-pipelined.csv
read-lock-bypass-rerun-gocache-memory.txt
baseline-rerun-gocache.csv            (third sample of post-bypass; misnamed)
baseline-rerun-gocache-pipelined.csv
baseline-rerun-gocache-memory.txt
cleanups-only-gocache.csv             (after reverting the bypass branch)
cleanups-only-gocache-pipelined.csv
cleanups-only-gocache-memory.txt
read-lock-bypass-summary.md           (this file)
```

Reproduce comparisons:

```bash
bench/redis-benchmark/compare.sh baseline-gocache read-lock-bypass-gocache       # the regression
bench/redis-benchmark/compare.sh baseline-gocache cleanups-only-gocache           # after revert (clean)
bench/redis-benchmark/compare.sh read-lock-bypass-gocache read-lock-bypass-rerun-gocache  # noise floor
```
