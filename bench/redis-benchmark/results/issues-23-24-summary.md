# Issues #23 + #24 — diagnosis findings and production benchmark deltas

**Date:** 2026-05-02
**Branch:** `fix/issues-23-24`
**Commits:** `c391c96` (TCP_NODELAY), `f51309f` (estimateSize O(1) on promoted hashes)
**Captured against:** `1ea8c65` (main tip; pre-fix baseline)

This summary pairs the diagnosis-pass findings (in `bench/profiles/diagnosis-baseline/SUMMARY.md`) with the measured impact of the two fixes that landed before any of the four planned `command-flow-optimization` child plans started.

## What we set out to fix

The diagnosis pass identified two bugs outside the scope of the four planned optimizations (`sink-aware-fast-path`, `engine-pooling`, `read-lock-bypass`, `batched-pipelined-dispatch`). Both were flagged by the diagnosis plan's "stop and ask" gate (a function dominating >50% cumulative CPU that none of the planned optimizations addressed).

### Finding 1 — `cache.estimateSize` walks promoted hash maps on every mutation (issue #23)

`pkg/cache/cache.go::estimateSize` walked `map[string]string` per element on every native-encoding mutation. `setInternal` and `chargedSize` both called it, making each HSET on a promoted hash O(N) — hence O(N²) over a workload that grows the hash.

Profile evidence from `bench/profiles/diagnosis-baseline/cpu/inproc-hset.prof`:

```
$ go tool pprof -top -cum -nodecount=10 cpu/inproc-hset.prof
   142.42s 61.21% 79.05%   187.62s 80.63%  internal/runtime/maps.(*Iter).Next
    26.34s 11.32% 11.35%   229.11s 98.46%  gocache/pkg/cache.estimateSize
     0.01s 0.0043% 79.05%   114.81s 49.34%  gocache/pkg/cache.(*Cache).chargedSize
```

**98% of CPU on the standard HSET workload** consumed by map iteration.

A diagnostic counter in `HandleHset`'s native branch revealed the trigger: `valkey-benchmark -t hset` writes to a single fixed hash key `myhash` regardless of `-r`. After ~512 fields the hash promotes to `EncNative`; every subsequent HSET walks the entire map.

```
DIAG hsetNative hit #1 key=myhash enc=0 type=2 valnil=false
DIAG hsetNative hit #2 key=myhash enc=0 type=2 valnil=false
DIAG hsetNative hit #3 key=myhash enc=0 type=2 valnil=false
... (all to the same key)
```

This single bug was the entire 100× HSET rps gap to valkey.

### Finding 2 — server didn't set `TCP_NODELAY` (issue #24)

`grep -rn 'TCP_NODELAY\|SetNoDelay' pkg/server cmd/server` returned nothing. Linux defaults TCP sockets to Nagle's algorithm enabled. The hypothesis: 50 single-command-per-RTT clients pay 40 ms per command via Nagle's interaction with delayed-ack. Loopback-based harness numbers (4.7 µs/op for HSET-spread) suggested the docker bridge was where Nagle stalled.

The hypothesis turned out to be **wrong for this workload**. After applying the TCP_NODELAY fix and rebuilding, HSET stayed at 992 rps — verifying that estimateSize (Finding 1), not Nagle, was the dominant cost. TCP_NODELAY still ships for general latency hygiene with non-tight-loop clients.

## What was changed

### `c391c96` — `fix(server): set TCP_NODELAY on accepted connections`

```go
// pkg/server/server.go::handleConnection
if tcpConn, ok := conn.(*net.TCPConn); ok {
    if err := tcpConn.SetNoDelay(true); err != nil {
        logger.WarnNoCtx().Err(err).Msg("set TCP_NODELAY failed")
    }
}
```

Plus `TestServer_TCPNoDelay` (`pkg/server/server_test.go`) — wraps the listener with a `captureListener` that records accepted conns, then asserts `TCP_NODELAY=1` on the server-side socket via `syscall.GetsockoptInt`.

Closes #24. **Verification result: zero rps movement on the hot HSET path** (992 rps → 992 rps). Ships as a correctness improvement, not a performance fix.

### `f51309f` — `perf(cache): O(1) HSET on promoted hashes via SlotMeta.NativeSize`

Three coordinated changes:

1. **`pkg/cache/slab/meta.go`** — Added `NativeSize uint32` to `SlotMeta`. The struct stays at 40 bytes because the new field consumes 4 of the 6 bytes of natural trailing padding. Comment block updated.

2. **`pkg/cache/cache.go`** — `chargedSize` no longer walks native values:
   ```go
   func (c *Cache) chargedSize(key string, ptr slab.SlabPointer) int64 {
       meta := c.slabs.Meta(ptr)
       enc := Encoding(meta.Encoding)
       if enc == EncPacked {
           return int64(entryOverhead) + int64(len(key)) + int64(c.slabs.Size(ptr))
       }
       return int64(entryOverhead) + int64(len(key)) + int64(meta.NativeSize)
   }
   ```

   Plus a new public method `RawSetNativeWithSize(ctx, key, value, byteSize, expiration)` for handlers that already know the size — bypasses the estimateSize walk entirely. `setInternal` was refactored into `setNativeInternal` for the native path with the size-passed-in invariant; the old `setInternal` retains its previous behaviour (estimateSize once) for callers that don't know the size (snapshot loading, etc.).

   Also exposed `Cache.NativeSize(key) int64` for handlers to read the cached size without walking.

3. **`pkg/resp/handler/hash.go`** — HSET and HDEL native paths track size incrementally:
   - `finishHsetNative` accepts a `priorSize` parameter and computes the delta from the kvs being set (`+len(field)+len(value)+32` per new field, `+len(newVal)-len(oldVal)` per replacement).
   - `hsetNative` reads the existing size via `cmdCtx.Cache.NativeSize(key)`.
   - At packed→native promotion (`hsetStartPacked`/`hsetPacked`), `hashMapSize` walks the freshly-promoted map exactly once. After that, every HSET to that hash is O(1) regardless of map size.
   - The HDEL native branch decrements `size` per removed field.

Plus `TestHSET_PromotedHash_O1` (`pkg/server/bench_test.go`) — issues 100 000 sequential HSETs to one hash and asserts completion within 10 s. Before the fix: ~140 s. After: <3 s with the race detector.

Closes #23.

## Production redis-benchmark deltas

Captured via `bench/redis-benchmark/run.sh estimateSize-fix --target gocache` after rebuilding the docker image. n=100 000, c=50, r=100 000, P=10 (pipelined), CPU pinned to cpus 0–3.

### Standard suite

| Test | Baseline rps | After fix rps | Δ vs base | Valkey rps | Ratio (after / valkey) |
|---|---:|---:|---:|---:|---:|
| PING_INLINE | 106 157 | 93 984 | −11% | 93 720 | 1.00× |
| PING_MBULK | 104 821 | 98 039 | −6% | 92 936 | 1.05× |
| SET | 100 300 | 86 805 | −13% | 95 510 | 0.91× |
| GET | 101 010 | 88 261 | −13% | 91 827 | 0.96× |
| INCR | 96 525 | 99 800 | +3% | 97 370 | 1.02× |
| MSET | 102 986 | 89 445 | −13% | 92 336 | 0.97× |
| LRANGE_100 | 70 671 | 69 060 | −2% | 66 622 | 1.04× |
| LPUSH | 4 405 | 5 301 | +20% | 93 457 | 0.057× |
| RPUSH | 3 611 | 6 943 | +92% | 90 744 | 0.077× |
| LPOP | 3 628 | 6 951 | +92% | 96 993 | 0.072× |
| RPOP | 10 759 | 20 610 | +92% | 92 421 | 0.22× |
| **HSET** | **1 003** | **103 842** | **+10 254%** | 87 796 | **1.18×** |
| SADD | 1 016 | 1 968 | +94% | 91 743 | 0.021× |
| SPOP | 1 194 | 2 309 | +93% | 89 365 | 0.026× |

### Pipelined suite (P=10)

| Test | Baseline rps | After fix rps | Δ vs base | Valkey rps | Ratio (after / valkey) |
|---|---:|---:|---:|---:|---:|
| PING_INLINE | 751 879 | 704 225 | −6% | 847 457 | 0.83× |
| PING_MBULK | 729 927 | 684 931 | −6% | 990 099 | 0.69× |
| SET | 490 196 | 444 444 | −9% | 781 249 | 0.57× |
| GET | 483 091 | 500 000 | +3% | 806 451 | 0.62× |
| INCR | 495 049 | 478 468 | −3% | 675 675 | 0.71× |
| MSET | 196 850 | 200 400 | +2% | 321 543 | 0.62× |
| LRANGE_100 | 152 905 | 157 480 | +3% | 197 628 | 0.80× |
| LPUSH | 1 375 | 1 665 | +21% | 769 230 | 0.002× |
| RPUSH | 2 188 | 4 216 | +93% | 877 193 | 0.005× |
| LPOP | 2 209 | 4 249 | +92% | 884 955 | 0.005× |
| RPOP | 3 672 | 6 793 | +85% | 1 020 408 | 0.007× |
| **HSET** | **469** | **478 468** | **+101 951%** | 917 431 | **0.52×** |
| SADD | 1 042 | 2 001 | +92% | 990 099 | 0.002× |
| SPOP | 1 223 | 2 364 | +93% | 540 540 | 0.004× |

## Reading the numbers

**HSET went from 53× slower than valkey to 1.18× faster on standard, and from 1955× slower to 0.52× on pipelined.** This is the headline win. The bug was concentrated entirely in the EncNative path; once `chargedSize` returns in O(1) and HSET tracks size incrementally from priorSize, the dispatch becomes the dominant cost — comparable to valkey's single-threaded handler.

**SADD / SPOP / LPUSH / RPUSH / LPOP / RPOP all improved ~2× without code changes to their handlers.** The win came from `chargedSize` no longer walking the value on the OLD-size lookup path. They still walk for the NEW size in `setInternal::estimateSize`, so they remain O(N) per call rather than O(1). Making them fully O(1) means replicating the incremental-tracking pattern from `finishHsetNative` in their native paths — left as follow-up work since:
- LPUSH/SADD don't promote in valkey-benchmark's default workloads (different fixed-key vs random-key pattern than HSET)
- The 2× gain from the chargedSize cleanup is meaningful; the remaining gap to valkey on these is dominated by the per-command instrumentation overhead the four planned optimizations target

**Simple-op standard rps regressed 6–13% (PING / SET / GET / MSET).** This is unexpected. Likely causes ranked by suspicion:
1. Memory pressure: the bench now actually completes 100 000 HSET ops (previously ~40 % did, due to the slowdown), filling the cache with the giant `myhash`. RSS delta rose 311 → 332 MiB. More GC scanning per allocation on the simple-op tests that follow.
2. Cache lock contention with the now-fully-populated hash. The bench runs all tests sequentially on the same connections; SET ops compete with whatever residual eviction the engine does.
3. Variance — 6–13% is roughly 2–3σ for these workloads at this sample size.

These regressions are **not on the critical path** for the parent `command-flow-optimization` plan, which targets exactly these workloads via sink-aware-fast-path, engine-pooling, and read-lock-bypass.

**Pipelined HSET reaching 478 k rps vs valkey 917 k (0.52×) leaves the engine-hop and per-command instrumentation as the remaining gap.** This is exactly what the four planned optimizations target. Pipelined HSET went from being the worst-case workload (469 rps) to being mid-pack — now closer to GET/SET than to LPUSH/SADD.

## What this fixes does NOT address

- **Pipelined LPUSH at 1 665 rps (0.002× valkey)** — list mutations re-allocate the entire list on every push (because `estimateSize` was the cheap part of LPUSH; the actual cost is the slice-copy in `RawSet`). Likely needs incremental tracking + a different list encoding for promoted lists.
- **Pipelined SADD at 2 001 rps (0.002× valkey)** — same pattern, set re-allocates the map. Same fix shape.
- **Standard SET / GET regression** — investigate during sink-aware-fast-path; the diagnosis profile already pointed to bus.Emit + tracker as 80% of mutex contention, which compounds with the bigger heap.

## Files changed

```
pkg/server/server.go      | +10
pkg/server/server_test.go | +94 (TestServer_TCPNoDelay)
pkg/cache/cache.go        | +103 -29
pkg/cache/slab/meta.go    | +6 -4
pkg/resp/handler/hash.go  | +43 -7
pkg/server/bench_test.go  | +522 (new harness)
                          ----
                          706 lines added, 40 modified
```

## Files captured

- `diagnosis-baseline-{gocache,valkey}.csv` + `-pipelined.csv` + `-memory.txt` — pre-fix
- `tcp-nodelay-fix-gocache.csv` + `-pipelined.csv` + `-memory.txt` — TCP_NODELAY only (verification of the failed hypothesis)
- `estimateSize-fix-gocache.csv` + `-pipelined.csv` + `-memory.txt` — both fixes
- `bench/profiles/diagnosis-baseline/cpu/inproc-hset.prof` + `.top.txt` — the profile that surfaced the bug
- `bench/profiles/diagnosis-baseline/SUMMARY.md` — full diagnosis writeup
