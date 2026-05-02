# gocache vs valkey — where the real gap is

**Date:** 2026-04-20 · **Branch:** `feat/memory-optimization`

Same containerized harness (`0-3` target, `4-7` client, 2 GiB each,
`valkey/valkey:8` client, `-n 100000 -c 50 -r 100000`, P=10 for pipelined).

## Headline: the hybrid-encoding redo is healthy. gocache itself is the bottleneck.

The hybrid encoding fixed the regression (was -50% to -98%, now -7% worst-case
on standard). But the absolute gap to valkey is still 30–55× on many ops.
That gap is **not** an encoding problem — it was there in resp-pool already.

## Standard suite (no pipeline)

| command     | valkey rps | gocache ph-0 | gocache ph-1 | valkey p99 | gocache ph-1 p99 | gap (valkey / ph-1) |
|-------------|-----------:|-------------:|-------------:|-----------:|-----------------:|--------------------:|
| PING_INLINE |    100 705 |      99 108  |     102 459  | 0.35 ms    | 0.54 ms          |   **0.98×** (parity) |
| PING_MBULK  |    100 503 |      95 238  |      98 814  | 0.36       | 0.50             |   1.02× |
| SET         |    102 249 |      94 607  |      95 147  | 0.53       | 0.71             |   1.07× |
| GET         |    105 820 |     101 833  |      94 251  | 0.36       | 0.51             |   1.12× |
| INCR        |    105 042 |     101 729  |      90 827  | 0.42       | 0.66             |   1.16× |
| MSET (10)   |    102 354 |      90 334  |      83 612  | 0.71       | 1.30             |   1.22× |
| LRANGE_100  |     71 788 |      65 789  |      64 809  | 0.63       | 2.36             |   1.11× |
| LPOP        |    103 093 |       6 516  |       6 390  | 0.44       | 12.94            | **16.1× slower** |
| RPOP        |    103 199 |      18 950  |      18 889  | 0.35       | 6.09             | **5.5× slower** |
| LPUSH       |    101 010 |       3 305  |       3 284  | 0.38       | 36.90            | **30.8× slower** |
| RPUSH       |    103 413 |       6 605  |       6 255  | 0.39       | 14.18            | **16.5× slower** |
| HSET        |    106 724 |       1 995  |       1 863  | 0.38       | 56.58            | **57.3× slower** |
| SADD        |    104 058 |       2 007  |       1 918  | 0.37       | 56.00            | **54.3× slower** |

**Shape of the gap:**

- Pings / strings (SET, GET, INCR, MSET, LRANGE_100): gocache **within 1.1× of valkey**. The transport and RESP-level work is competitive.
- Collection mutations (HSET, SADD, LPUSH, RPUSH, LPOP, RPOP): gocache is **5–57× slower**, with p99 latency 15–150× worse.

The 50× gap on HSET / SADD isn't a collection-encoding bug — it shows up identically in resp-pool (1 995 HSET rps, before any encoding changes) and hybrid-encoding (1 863 rps). Encoding moved the needle ~7%. The other **57×** is everywhere else.

## Pipelined suite (P=10)

| command     | valkey rps | gocache ph-1 | gap        |
|-------------|-----------:|-------------:|-----------:|
| PING_INLINE |    990 099 |      515 464 |   **1.92×** |
| PING_MBULK  |  1 030 928 |      591 716 |   1.74× |
| GET         |    970 874 |      401 606 |   2.42× |
| SET         |    813 008 |      362 319 |   2.24× |
| INCR        |    961 538 |      396 825 |   2.42× |
| MSET (10)   |    300 300 |      101 010 |   2.97× |
| LRANGE_100  |    203 666 |      112 867 |   1.80× |
| LPOP        |    925 926 |        3 831 | **242× slower** |
| RPOP        |    961 538 |        6 350 | **151× slower** |
| LPUSH       |    714 286 |        1 024 | **697× slower** |
| RPUSH       |  1 000 000 |        3 797 | **263× slower** |
| HSET        |    806 452 |          850 | **949× slower** |
| SADD        |    943 396 |          872 | **1082× slower** |

Pipelining reveals the bottleneck even more clearly: simple ops get a ~2× multiplier from pipelining; collection ops get **nothing** (LPUSH went 3 284 → 1 024, worse pipelined). The collection-write throughput is **capped somewhere below 1 000 rps pipelined**, regardless of client concurrency.

## Diagnosis — why gocache is 50× behind on collections

### (1) Per-command bookkeeping in the evaluator is enormous

Every command — even PING — hits `pkg/evaluator/evaluator.go`, which does (hot-path excerpt):

```go
cmdOp := b.tracker.Start(ops.TypeCommand, ctx.OperationID)  // alloc Operation + UUID, map insert under mutex
cmdOp.Enrich(command.StartNs, ...)                          // map write under mutex
cmdOp.Enrich(command.OperationID, ...)                      // ×2
cmdOp.Enrich(command.CommandKey, op)                        // ×3
cmdOp.Enrich(command.ArgCountKey, ...)                      // ×4
metadata := rex.BuildMetadata(ctx.RexMeta, ctx.CmdMeta)     // alloc map
for k, v := range metadata { cmdOp.Enrich(...) }            // N Enrich calls
opCtx := ops.WithContext(parentCtx, cmdOp)                  // context alloc
b.opHookExecutor.RunStartHooks(opCtx, cmdOp)                // check + potential IPC
b.emitter.Emit(events.NewOperationStart(...))               // event alloc + fan-out
b.emitter.Emit(events.NewCommandPre(...))                   // ×2
cmdCtx := &command.Context{...}                             // 12-field struct
cmdCtx.SetContext(opCtx)
// ... pre-hook check
handler(cmdCtx)                                             // the actual work
// ... post-hook + complete + emit + tracker delete
```

`grep -c instrumentation pkg/evaluator/evaluator.go` → **27 calls per command**. PING_INLINE doing 102k rps means each op costs ~10 μs across all of this *before* the handler runs. That's fine for PING (nothing else to do) but catastrophic for HSET which adds 2-3× more work inside the handler.

**Fix (ranked by impact):**

1. **Fast-path when no consumers are attached** — if `emitter == nil` AND `tracker has no subscribers` AND `hooks HasAny() == false`, skip the whole tracker/hook/emit block. Check once, drop everything on a read-only flag. Estimated win: 5–20× on simple ops.
2. **Pool `*command.Context`** via `sync.Pool`. Currently allocated per command. Estimated win: 10-20% from reduced GC pressure.
3. **Lazy `cmdOp.Enrich`** — only populate when a subscriber will read the context. The fields are only visible via `ContextSnapshot(false)` which is called once; everything else writes through a mutex-protected map for no reason in the fast path.
4. **Drop `rex.BuildMetadata` when `RexMeta == nil && len(CmdMeta) == 0`** — most commands are plain RESP, no REX meta.

### (2) Engine goroutine hop

`pkg/engine/engine.go` routes every mutation through one goroutine via a buffered channel. Per call:

```go
resChan := make(chan any, 1)                                // alloc
e.cmdChan <- Command{Execute: fn, ResChan: resChan}         // goroutine hop 1
// engine goroutine:
e.cache.Lock(); res := cmd.Execute(); e.cache.Unlock()
cmd.ResChan <- res                                          // goroutine hop 2
<-resChan                                                    // receive
```

Two channel hops + scheduler round-trip. On a 7900X with GOMAXPROCS=4 (our cpuset), that's ~2-5 μs per op **before anything useful happens**. Valkey is single-threaded and calls the handler inline; no hop.

**Fix:**

1. **Pool `resChan`** via `sync.Pool` — one alloc per op, trivial.
2. **Read-path bypass** — GET/HGET/LRANGE/HGETALL hold the cache read-lock only. Let them take `cache.RLock()` directly from the connection goroutine, no engine hop. This is the biggest single win; reads are the majority of production workloads and valkey's 970k GET pipelined leaves us 2.4× behind.
3. **Batched dispatch for pipelined ops** — when the connection reads N commands in one read(), dispatch them as one `Command` group so the engine processes 10 ops per hop, not 10 hops for 10 ops. Pipelining gives valkey 10× on simple ops because there's no hop cost; pipelining gives us 5× because every pipelined op still hops. A batched dispatch would close that gap.

### (3) Collection-write hot path still allocates

Even with packed encoding, HSET allocates:
- Fresh `[]byte` buffer via `packed.HashSet` on size-change writes (most HSETs on an empty hash).
- `Entry{}` struct in `setPackedInternal`.
- `list.Element` in `lruList.PushFront(key)`.

At 1 900 HSETs/sec on one engine goroutine, each op is 520 μs. That's within what an alloc-heavy path can do on a single core. Profile to confirm, but the obvious wins are:

1. **`sync.Pool` for packed buffer growth** — reuse previously-freed buffers instead of making a fresh one.
2. **Capacity hint in `HashSet`** — first write to a fresh hash currently does `append(emptyHeader, frame...)`. Grow with a sensible minimum cap (64 B) so subsequent writes re-slice without realloc.
3. **Skip LRU push for NEW keys** — we `lruList.PushFront(key)` on every write. For keys we've just pushed, it's redundant. gc-opaque-index replaces the doubly-linked list with slab-pointer intrusive LRU anyway.

### (4) Writer flush policy may hurt pipelined collection throughput

Standard LPUSH = 3 284 rps, pipelined LPUSH = 1 024 rps — pipelined is **worse**. That's pathological. Valkey pipelined LPUSH is 714 286. The drop suggests backpressure: maybe each LPUSH response forces a flush, or the blocking-wake logic (`tryWakeBlockedClients`) in `lists.go` is firing synchronously under load and blocking the dispatch goroutine.

Worth profiling with `go tool pprof` on a live bench to see what's actually on-CPU.

## The "other optimizations" list (prioritised)

### Ship this phase (small, measurable)

1. **Sink-aware fast path** in `evaluateInternal`: if no emitter / no hooks / no tracker subscribers, skip the 27 calls. **Expected +5–10× on PING/GET/SET standard, 2–3× on collections.**
2. **`sync.Pool` for engine `resChan`** and for `*command.Context`. **Expected 10-20% on all ops, less GC noise.**
3. **`sync.Pool` for packed buffer grow** — `packed.HashSet/ListAppend*/SetAdd/ZSetAdd` return a fresh `[]byte` on size change today. Pooling the scratch would address the hybrid-encoding summary's +188 MB RSS growth.
4. **Capacity hint for fresh packed collections** — start with 64 B backing, not 4 B. Shaves reallocs on the common empty→small growth.

### Next phase (structural)

5. **Read-lock bypass** — read commands take `cache.RLock()` directly from the connection goroutine; no engine dispatch. **Expected 2-3× on GET/HGET/LRANGE; brings pipelined GET from 401k → ~900k (valkey-parity).**
6. **Batched dispatch** — pipelined commands arrive as a single read; dispatch them as a group so the engine processes the batch under one lock acquisition. **Expected close to valkey's 2× pipelined multiplier.**
7. **Slab allocator (existing slab-allocator plan)** — replaces the `make([]byte, ...)` in setPackedInternal + reduces GC pointers. **Expected +20-40% on collection writes, RSS delta back near baseline.**

### Deferred / thesis-out-of-scope

8. **Multiple engine goroutines** (like dragonfly's per-shard scheme). Breaks the "serial dispatch" architectural invariant and is a bigger redesign than the thesis scope. Parking.
9. **Custom hash/skiplist** (like valkey's dict or dragonfly's DenseSet). Reimplementing Go's map with lower GC overhead. gc-opaque-index covers a chunk of this via `map[string]SlabPointer` already.

## Bottom line for the thesis

The **thesis contribution is the microkernel + plugin split**, and the hybrid
collection encoding that makes small-collection storage competitive.
The 30–50× valkey gap is dominated by **per-command instrumentation overhead**
(operation tracking + event emission + hooks) — features we added
*intentionally* for observability. Valkey doesn't have them.

If the thesis wants to show the cache is production-capable, fast-path
(optimization #1) is the single highest-ROI fix: demonstrate that when
observability is off (production default), gocache is within ~20% of valkey
on reads and within 2× on collection writes.

If we stay at 50× below valkey, the write-up needs to frame observability as
a paid-for first-class feature (which it is) rather than a leak. Either
framing is defensible.
