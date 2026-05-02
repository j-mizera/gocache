# Diagnosis pass — synthesis

**Date:** 2026-05-02
**Branch:** `fix/issues-23-24` (no source changes vs main `1ea8c65`)
**Harness:** `pkg/server/bench_test.go` — both in-process and TCP-loopback variants, 52 parallel goroutines (`SetParallelism(13)` × `-cpu=4`), 10 s benchtime, profile sampling at `mem=4096 / block=10000ns / mutex=1/10`. Emitter wired (matches production binary).

The deliverable: validate or refute each of the four child plans' premises with profile evidence and surface bottlenecks the plans don't address.

## Headline rps (52 goroutines, in-process bypasses TCP+RESP)

| Workload | Without emitter | With emitter | Production baseline (50 client docker) |
|---|---:|---:|---:|
| InProc HSET (rotating fields, **promoted to native**) | 1 406 µs/op | 1 229 µs/op | n/a (synthetic) |
| **InProc HSET spread (100k hashes, 1 field, packed)** | 1 092 ns/op | 1 436 ns/op | n/a |
| InProc GET | 1 438 ns/op | 1 957 ns/op | n/a |
| InProc SET | 1 867 ns/op | 2 102 ns/op | n/a |
| TCP HSET std (rotating, native) | 1 344 µs/op | 1 260 µs/op | n/a |
| TCP HSET std (spread, packed) | 4 544 ns/op | 4 698 ns/op | 1 003 rps = 49.8 ms / op (**~10 600× slower**) |
| TCP GET std | 5 192 ns/op | 5 501 ns/op | 91 827 rps = 10.9 µs / op (~2× harness) |
| TCP SET std | 5 095 ns/op | 5 436 ns/op | 95 510 rps = 10.5 µs / op (~2× harness) |
| TCP GET pipe (P=10) | 22.1 µs / batch | 27.9 µs / batch | 483 091 rps = 2.07 µs / cmd |
| TCP HSET pipe (P=10) | 7.1 ms / batch | 6.9 ms / batch | 469 rps |

The harness measures the *raw dispatch + cache cost in isolation*. The production benchmark sees additional latency from the docker bridge network and (likely) Nagle's algorithm — see Finding 2.

## Findings

### Finding 1 — `cache.estimateSize` walks promoted hash maps on every mutation

**Severity:** CRITICAL — explains ~98 % of CPU on rotating-field HSET; not predicted by any of the four child plans.

`pkg/cache/cache.go:705` defines `estimateSize` which falls through to a per-element walk of `map[string]string` / `map[string]struct{}` / `*SortedSet` for EncNative entries. `setInternal` calls it on every mutation. For a hash that has promoted past the packed-encoding threshold (~128 fields), every HSET pays O(N) — making the workload O(N²) overall.

The slab-allocator plan claimed Phase 1 deleted `estimateSize`, but the function survived for the EncNative (promoted) path because `chargedSize` (line 658) cannot derive size from the slab capacity for native values and falls back to `estimateSize`.

**Evidence:** `cpu/inproc-hset.top.txt` (rotating fields, native): `internal/runtime/maps.(*Iter).Next` = 80 %, `cache.estimateSize` = 99 % cum. `cpu/inproc-hset-spread.top.txt` (100 k hashes, packed): the entire cost moves to dispatch + emitter, no `estimateSize` in top-20.

**Production exposure:** redis-benchmark's `-t hset` writes 100 k unique hashes with one field each. Hashes never promote → `estimateSize` is called only on packed values where it returns `len(payload)` (O(1)). The production HSET-at-1003 rps is bottlenecked elsewhere — almost certainly Finding 2.

**Fix scope:** small. Either (a) track encoded byte length in `SlotMeta` for native entries and return it directly from `chargedSize`, or (b) make `setInternal` accept the size from the handler (which already knows it because it just encoded the value). Either is a one-day task and should ship as its own commit, not part of any of the four child plans.

### Finding 2 — Server does not set `TCP_NODELAY` on accepted connections

**Severity:** HIGH — most likely explanation for the 10 600× rps gap between the harness (TCP loopback) and the production benchmark (docker bridge).

`grep -rn TCP_NODELAY pkg/server/ cmd/server/` returns nothing. Go's `net.Conn` from `Listener.Accept()` defaults to whatever the OS sets, which on Linux is **Nagle's algorithm enabled**. Combined with the kernel's delayed-ack timer, single-command-per-RTT clients (the production redis-benchmark standard suite) pay up to 40 ms per round trip when the response packet doesn't fit a partial buffer.

**Evidence:** the harness over TCP loopback measures HSET-spread at 4.7 µs / op. Production with the docker bridge measures 49.8 ms / op. Loopback bypasses real TCP segmentation so Nagle has no effect there; the docker bridge passes through the kernel's networking stack and does. The 10 600× gap is the order of magnitude expected from per-op Nagle stalls.

**Verification needed:** patch `srv.handleConnection` to call `tcpConn.SetNoDelay(true)` on the accepted connection, rebuild the docker image, re-run `bench/redis-benchmark/run.sh`. Expected: standard HSET jumps from 1003 rps to ≥40 k rps.

**Production exposure:** dominates standard-mode collection writes. Pipelined mode amortises the Nagle delay across the batch but doesn't fully hide it. Pipelined HSET pessimisation (469 rps < 1003 standard) is consistent with Nagle interacting badly with the engine queue's per-batch synchronous serialisation.

**Fix scope:** one-line patch in `pkg/server/server.go::handleConnection`. Should ship before the four child plans land — every subsequent benchmark would otherwise be Nagle-bound.

### Finding 3 — `events.(*Bus).Emit` is the dominant lock-contention source on simple writes (sink-aware-fast-path's premise, confirmed)

**Severity:** HIGH — confirms the sink-aware-fast-path plan and quantifies the upside.

`mutex/inproc-set.top.txt`: 81.5 % of mutex contention is `sync.Mutex.Unlock`. Decomposed:
- `events.(*Bus).Emit` — **40.6 %** of total mutex time (bus.mu acquired per emit to push the replay ring + check subscribers)
- `operations.(*Tracker).Complete` — **24.4 %**
- `operations.(*Tracker).Start` — **15.3 %**
- Subtotal of the three sinks: **80.3 %** of mutex contention

The bus has zero subscribers in the harness (and in a no-plugin production deployment), so every `Emit` is purely overhead. The tracker registers operations into a `map[string]*Operation` even when no subscriber will ever observe them.

**Verification:** the InProc-SET benchmark without the emitter wired ran at 1 867 ns/op. With the emitter wired: 2 102 ns/op. Delta = +13 %. Removing the tracker too would close the rest. The full-sink-aware version would land in the 1.4–1.7 µs/op range, an estimated **+25–35 %** rps improvement on simple writes (in-process).

**Production scaling:** the per-op cost is constant; the relative impact on production benchmarks depends on what else the workload is bottlenecked on. With Finding 2 fixed (Nagle removed), sink-aware should add a clean 25–35 % on top.

### Finding 4 — Engine channel hop dominates the wait time on simple-op block profiles (engine-pooling + read-lock-bypass premise, confirmed)

**Severity:** MEDIUM — confirms two of the four plans.

`block/inproc-set.top.txt`: **79 %** of blocking time is `runtime.selectgo` reached via `engine.sendAndWait`. Same story for `block/inproc-get.top.txt`. The connection goroutine submits a Command struct to `engine.cmdChan`, parks on `resChan`, gets unparked when the engine writes the result, parks again. Two scheduler hops + channel send/receive per command.

`block/tcp-get-pipe.top.txt`: same dominant `selectgo` from `sendAndWait`, but ratio drops to 42 % because `handleConnection` blocking on the connection read also contributes (25 %).

**Verification:** the engine-pooling plan's claim of 10–20 % uniform improvement is consistent with eliminating the `make(chan any, 1)` per submission. The read-lock-bypass plan's claim of 2–3× pipelined GET is consistent with removing the channel hop entirely for read-only commands — the block profile shows the hop cost is a concrete plurality of the wait time.

### Finding 5 — `resultToHookStrings` calls `fmt.Sprintf("%v", v)` on every command's return value

**Severity:** LOW — modest but easy fix.

`cpu/inproc-get.top.txt`: `fmt.Sprintf` = 14.5 %, `resultToHookStrings` = 12.95 % cum. The function runs unconditionally on every command (lines 249, 256 in `evaluator.go`) to format the result for hook context AND for `cmdOp.Enrich(ResultKey, ...)`. When sink-aware-fast-path bypasses both branches, `resultToHookStrings` becomes dead code on the fast path.

Folded into the sink-aware-fast-path plan — no separate work needed.

### Finding 6 — TCP standard-mode workloads are bottlenecked by the connection layer, not dispatch

**Severity:** INFO — frames where the cost lives in production.

`cpu/tcp-get-std.top.txt`: 54 % of CPU is `internal/runtime/syscall.Syscall6` — Linux kernel TCP I/O. 39 % is `bufio.(*Writer).Flush`. The cache + evaluator path is <10 %.

**Implication:** the four child plans target dispatch and instrumentation overhead. For TCP standard-mode reads, the wins from those plans will be modest (5–15 %) because the dispatch is not the bottleneck — the kernel TCP stack is. Combined with Finding 2 (Nagle), this suggests the TCP standard-mode gap to valkey is dominated by **Nagle + connection-per-syscall overhead**, not by dispatch architecture.

## Per-plan attribution and revised projections

| Plan | Profile evidence | Original projection | Revised projection | Confidence |
|---|---|---|---|---|
| sink-aware-fast-path | bus.Emit + tracker = 80 % of mutex contention; bypassing them removes 25–35 % cost on simple workloads (in-process measured) | 5–10× HSET, +13 % uniform | +25–35 % rps on simple in-process; +10–15 % on TCP standard (kernel cost dominates); HSET behaviour depends on Findings 1+2 | **high** for simple writes; **medium** for HSET (Findings 1+2 are the bigger levers) |
| engine-pooling | `make(chan any, 1)` and `*command.Context` heap allocs visible in alloc/inproc-set.objects.txt; combined with engine-hop block-profile dominance | +10–20 % uniform | +10–15 % uniform; allocator-pool gain is real but the channel hop itself remains | **medium** |
| read-lock-bypass | engine channel block dominates 42 % of pipelined GET wait time; bypass removes the hop entirely for read-only commands | pipelined GET → 0.95×+ valkey | pipelined GET → ~0.85× valkey; the kernel-syscall ceiling at ~50 % CPU sets a hard upper bound | **medium-high** |
| batched-pipelined-dispatch | engine-hop cost amortised by batching; the marginal win after sink-aware + read-lock is modest | parked, deprioritised | confirmed deprioritisation; the per-op work is already small enough that batching is single-digit-percent | **low** |

## Recommended sequencing — REVISED from parent plan

1. **Finding 2 fix (TCP_NODELAY)** — one-line patch, must land first or every subsequent measurement is Nagle-bound. **This was not in any plan.**
2. **Finding 1 fix (estimateSize on native)** — avoids the O(N²) regression for any workload that promotes a hash. **Also not in any plan.** Pairs naturally with the slab-allocator follow-up work.
3. **sink-aware-fast-path** — biggest lever after the two bugs are fixed. Profile-confirmed.
4. **engine-pooling** — small uniform win, low risk, ship anytime.
5. **read-lock-bypass** — meaningful pipelined-read gain, kernel-syscall ceiling is the real cap.
6. **batched-pipelined-dispatch** — re-evaluate after the above; expected to provide single-digit additional improvement.

The original parent plan ordered (1) sink-aware, (2) engine-pooling, (3) read-lock-bypass, (4) batched. **Findings 1 and 2 invalidate that order** — both are bigger wins for collection-write workloads than any of the four plans.

## Predicted post-all-fixes numbers

Predictions assume all of: TCP_NODELAY enabled, estimateSize O(1) for native, sink-aware-fast-path, engine-pooling, read-lock-bypass. Numbers are rps on the production redis-benchmark harness vs the captured baseline.

| Workload | Baseline | Post-all | Confidence range |
|---|---:|---:|---|
| Standard SET | 100 300 | 130 000–170 000 | medium |
| Standard GET | 101 010 | 130 000–170 000 | medium |
| Standard HSET (spread) | 1 003 | **40 000–70 000** | medium (TCP_NODELAY is the lever) |
| Standard SADD/SPOP | 1 016 / 1 194 | 40 000–70 000 | medium |
| Pipelined GET | 483 091 | 700 000–850 000 | medium-high |
| Pipelined SET | 490 196 | 600 000–750 000 | medium |
| Pipelined HSET (spread) | 469 | 50 000–100 000 | low (workload behaves nonlinearly) |

Pipelined HSET's prediction is the widest because (a) pipelined collection writes were already at <0.5 % of valkey and (b) the interaction between Nagle, the engine queue, and the cache lock is non-obvious until measured.

## What this diagnosis did NOT do

- **Upper-bound stub experiments** were skipped to keep the pass focused; the no-emitter run effectively gave us a partial upper bound for sink-aware. Full stubs would tighten the projections by ±10 %.
- **`runtime/trace` analysis** captured but not yet visualised — would help understand the goroutine-park pattern in pipelined workloads. Worth a follow-up if the pipelined-HSET projection turns out to be wrong.
- **Production-shape redis-benchmark with the TCP_NODELAY patch** is the most important verification step and was not in this pass — it should be the first thing run before sink-aware-fast-path lands.

## Files

- `cpu/{label}.prof` + `.top.txt` — CPU profiles
- `alloc/{label}.mem` + `.objects.txt` + `.space.txt` — allocation profiles
- `block/{label}.prof` + `.top.txt` — block profiles
- `mutex/{label}.prof` + `.top.txt` — mutex contention profiles
- `gctrace/{label}.gctrace` — GC trace logs
- `trace/tcp-{get,hset}-pipe.trace` — runtime traces (10 s, pipelined only)

Labels: `inproc-{hset,get,set}`, `inproc-hset-spread`, `tcp-{hset,get,set}-std`, `tcp-hset-spread`, `tcp-{get,hset}-pipe`.
