---
title: Codebase audit — 42K lines, 54 packages
description: 5-agent parallel audit findings, fixes applied, and deferred items
status: complete
last_updated: 2026-05-26
related:
  - ADR-0021
  - Server-Architecture
  - Plugins
---

# Codebase Audit — 2026-05-26

5-agent parallel audit of gocache (42K lines, 54 packages). Agents covered: correctness/races, plugin lifecycle, layering/architecture, performance, and security. Findings below are tagged by severity (H = high, M = medium) and grouped by the commit that fixed them.

## Fixed — Commit 1: correctness bugs and race conditions

| ID | Severity | Finding | File | Fix |
|----|----------|---------|------|-----|
| H1 | HIGH | SET XX stale `found` after lazy expiry — expired key deleted by NX path but `found` remains true, so `xx && !found` incorrectly accepts | `pkg/resp/handler/basic.go` | Reset `found = false` after lazy-expiry delete |
| H2 | HIGH | OnMutate data race — `OnMutate`/`OnMutateAll` are plain func fields written by one goroutine, read by shard goroutines | `pkg/cache/cache.go` | Changed to `atomic.Pointer[func(...)]` with setter methods; callers load atomically |
| H3 | HIGH | AOFSource.SetPath race — `SetPath` writes path field concurrently with `Boot` reading it | `plugins/aof/reader.go` | Added `sync.RWMutex`; Lock in SetPath, RLock+copy in Boot |
| H4 | HIGH | PUBLISH returns string instead of integer — RESP protocol violation | `plugins/pubsub/main.go` | Changed `Value: strconv.Itoa(len(matches))` → `Value: len(matches)` |
| H5 | HIGH | runBatch write errors ignored — write failures to client connections silently dropped | `pkg/server/server.go` | Propagate write errors: break from result loop on first failure |
| H6 | HIGH | ApplyMutation silently fails on packed encoding — type assertions fail on packed entries without promotion | `pkg/cache/store.go` | Check `entry.Encoding == EncPacked` and promote to native before type assertion in HDEL/SREM/SPOP/ZREM branches; fix getHash/getSet/getSortedSet helpers |
| H17 | HIGH | AcquireShard bypasses stopped check — shard acquisition succeeds on a stopped engine | `pkg/engine/engine.go` | Added `if e.stopped.Load() { return func(){} }` guard |
| H19 | HIGH | Slab index overflow — no guard against exceeding 24-bit slab index space | `pkg/cache/slab/class.go` | Added `if uint32(len(c.slabs)) >= (1 << 24)` panic guard in growOneSlab |
| M1 | MEDIUM | OBJECT ENCODING wrong for packed — returns hash/set/zset encoding names instead of "listpack" | `pkg/resp/handler/keys.go` | Check `entry.Encoding == cache.EncPacked` first, return `"listpack"` |
| M2 | MEDIUM | Dead test assertion — wrong condition in router test for unregistered command | `pkg/plugin/router/router_test.go` | Fixed to `if r.HasCommand("PUBLISH") { t.Error(...) }` |

## Fixed — Commit 2: plugin system lifecycle and reliability

| ID | Severity | Finding | File | Fix |
|----|----------|---------|------|-----|
| H7 | HIGH | Emit drops mutations silently — mutation feed drops with no observability | `pkg/persistence/feed.go`, `coordinator.go` | Added `droppedMutations atomic.Uint64` to Coordinator with getter |
| H8 | HIGH | Shutdown goroutine leak — `cmd.Wait()` goroutine not tracked by WaitGroup | `pkg/plugin/manager/manager.go` | Added `m.wg.Add(1)` + `defer m.wg.Done()` |
| H9 | HIGH | Double crash event — both readLoop and handlePluginExit emit crash events | `pkg/plugin/manager/manager.go` | Removed crash event emission from readLoop; only handlePluginExit emits |
| H10 | HIGH | Handshake cleanup incomplete — manual cleanup misses subsystems on handshake failure | `pkg/plugin/manager/manager.go` | Replaced manual cleanup with `m.deregisterPlugin(reg.Name)` |
| H18 | HIGH | BLPOP wake uses bulk lock — DispatchWithResult acquires all shards instead of target shard | `pkg/resp/handler/lists.go` | Replaced with `DispatchToShard(ctx, cache.ShardIndexOf(key), ...)` |
| H20 | HIGH | Inflight span map unbounded — tracer map grows without bound on abandoned operations | former observability tracer | Resolved by removing runtime tracing from the Prometheus metrics plugin; future `instrumentation` plugin must carry bounded inflight state from the start |
| M3 | MEDIUM | SDK session unbounded context — `context.Background()` used for gRPC calls with no timeout | `sdk/pluginsdk/session.go` | Replaced with `context.WithTimeout(context.Background(), 5*time.Second)` |
| M4 | MEDIUM | Bridge alias removal — deprecated bridge functions in api/command/hookctx.go | `api/command/hookctx.go` | Deleted SharedPrefix, NewHookCtx, MergeHookCtx, FilterHookCtx; updated caller to import api/context directly |
| M5 | MEDIUM | PushToClient error logging — write errors to client connections silently discarded | `plugins/pubsub/main.go` | Log errors at debug level instead of `_ =` |

## Fixed — Commit 3: introduce commons/ package layer (ADR-0021)

| ID | Severity | Finding | File | Fix |
|----|----------|---------|------|-----|
| M6 | MEDIUM | api/ contains concrete implementations — logger (191 LOC), crashdump (214 LOC), transport (143 LOC), RESP encoding violate api/ contract | `api/logger/`, `api/crashdump/`, `api/transport/`, `api/resp/` | Moved to `commons/` layer (ADR-0021) |
| M7 | MEDIUM | RESP type constant duplication — `api/resp/encode.go` lines 5-17 duplicate `pkg/resp/resp.go` | `api/resp/encode.go`, `pkg/resp/resp.go` | Consolidated into `commons/resp/`; removed duplicates |
| M8 | MEDIUM | Plugins can't access RESP helpers — isolation rule blocks `plugins/ → pkg/` but command constants + Value constructors live in `pkg/resp/` | `pkg/resp/` | Moved to `commons/resp/`, now importable by plugins |
| M9 | MEDIUM | PluginConfig concrete impl in api/ — MapConfig + provider mechanism is implementation, not contract | `api/config/` | Extracted to `commons/plugincfg/` |
| M10 | MEDIUM | Embedded plugin lifecycle in api/ — Plugin interface + registry is plugin-author plumbing, not server contract | `api/embedded/` | Moved to `sdk/embedded/` |
| M11 | MEDIUM | Test utility in api/ — MemoryStore (551 LOC) is test-only, inflates api/ surface | `api/persistence/memstore.go` | Moved to `testkit/memstore/` |
| M12 | MEDIUM | CI isolation script incomplete — only checks `plugins/ → pkg/`; missing api/, commons/, sdk/ rules | `scripts/check-plugin-isolation.sh` | Extended to enforce full layering for all 7 directories |

## Deferred to follow-up PRs

| ID | Severity | Finding | Reason Deferred |
|----|----------|---------|-----------------|
| H11 | HIGH | StateRegistered shutdown window — plugin can receive commands between registration and full readiness | Needs lifecycle state machine redesign |
| H21 | HIGH | Decompose manager.go (813 lines → 5 files) | Structural refactor, needs own PR |
| H23 | HIGH | Extract evaluateCore sub-functions | Structural refactor, needs own PR |
| M | MEDIUM | `api/operations` → OperationView interface extraction | Needs ADR; tight coupling with context propagation |
| M | MEDIUM | `api/persistence/registry.go` global registry → move to pkg/ | Needs ADR; changes init-time contract |
| M | MEDIUM | Logger interface extraction (define in api/, impl in commons/) | Multi-day effort needing own ADR |
| M | MEDIUM | Transport interface extraction (define in api/, impl in commons/) | Multi-day effort needing own ADR |
| M | MEDIUM | `sdk/pluginsdk/sdk.go` Run() decomposition (317 lines) | Structural refactor |
| M | MEDIUM | `PluginConn.Send` unbounded goroutine-per-send | Needs worker loop pattern; risk of behavior change |
| M | MEDIUM | Pub/Sub pattern matching exponential backtracking | Performance; needs regex engine replacement or memoization |
| M | MEDIUM | SortedSet.Rank O(N log N) per call | Performance; needs skip list or indexed tree |
| M | MEDIUM | MSET pool 256-shard hardcode | Config-driven shard count not plumbed to pool |

## Methodology

Five specialized agents ran in parallel, each covering a distinct concern:

1. **Correctness & races** — data races, protocol violations, silent failures
2. **Plugin lifecycle** — goroutine leaks, cleanup completeness, crash handling
3. **Layering & architecture** — import violations, misplaced implementations, duplication
4. **Performance** — algorithmic complexity, lock contention, unbounded growth
5. **Security** — input validation, resource exhaustion, error exposure

Each agent reviewed the full codebase independently. Findings were deduplicated, severity-ranked, and triaged into fixable-now vs. deferred based on blast radius and ADR requirements.
