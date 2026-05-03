---
title: ClientContext cross-goroutine audit
description: Race-detector audit of every ClientContext field — surfaced from #32, no new races found beyond the watchDirty fix
status: stable
last_updated: 2026-05-03
related:
  - Performance
  - Audit-per-shard-arc-summary
  - Server-Architecture
---

# ClientContext cross-goroutine race audit

**Issue:** [#35](https://github.com/j-mizera/gocache/issues/35)
**Trigger:** PR #32 surfaced a pre-existing race on `ClientContext.WatchDirty` —
written from the engine goroutine (under `cache.Lock` via
`watch.Manager.NotifyMutation`), read from the connection goroutine
(`HandleExec`) without the lock. Race existed in main; no test
exercised it. Fixed in #32 by converting the field to `atomic.Bool`.

This audit sweeps the remaining `ClientContext` fields for the same
shape of bug.

## Method

For each field, identify:

1. **Writers** — which functions write the field, on which goroutine.
2. **Readers** — which functions read the field, on which goroutine.
3. **Synchronization edges** — what happens-before relationship (if any)
   exists between writer and reader.

The Go memory model gives us happens-before via:
- channel send/receive (engine `cmdChan` ↔ `resChan` is the main one)
- mutex Lock/Unlock pairs
- atomic operations
- single-goroutine program order

A field is racy if a writer in goroutine A and a reader in goroutine B
have no synchronization edge between them.

## Findings

### Field-by-field analysis

| Field | Writers | Readers | Synchronization | Race? |
|---|---|---|---|---|
| `InTransaction` | `transaction.Multi/Discard`, `HandleExec` (all engine goroutine) | `evaluateInternal:184` (connection), `HandleExec`, `HandleWatch`, `transaction.Manager.{Multi,Discard}` (engine) | engine→connection via `resChan` recv (HB edge); engine self-reads same-goroutine | **no** |
| `CommandQueue` | `HandleMulti`/`StartTransaction` (engine), `EnqueueCommand` from `evaluateInternal:189` (connection!), `HandleExec`/`ResetTransaction` (engine) | `HandleExec` (engine) reads + nils | Connection appends only when `InTransaction=true` (engine just set it); engine reads queue only on EXEC (connection just sent it). All chains go through `cmdChan`/`resChan` ⇒ HB | **no** |
| `Authenticated` | `HandleAuth`, `HandleHello` (engine) | `server.go:334` auth gate (connection) | engine→connection via `resChan` recv | **no** |
| `ProtoVersion` | `HandleHello` (engine) | `server.go:388` `mapValueToResp` (connection), `HandleHello` self (engine) | engine→connection via `resChan` recv | **no** |
| `RexVersion` | `HandleHello` (engine) | `server.go:297` META gate (connection), `HandleHello` self (engine) | engine→connection via `resChan` recv | **no** |
| `RexMeta` | `ensureRexStore` (engine, via REX.META subcommands) | `evaluator.go:230,231,250,307` (engine, instrumentation slow path), `routeToPlugin` (connection via `evaluateInternal`) | engine self-reads same-goroutine; connection reads after engine cycle (HB via `resChan`) | **no** |
| `CmdMeta` | `server.go:350,352` (connection) | `evaluator.go:230,231,250,307` (engine instrumentation, but called via `evaluateInternal` which runs on connection goroutine) | All accesses on connection goroutine | **no** |
| `OperationID` | `server.go:226` (connection, ONCE at startup) | many (both goroutines) | Set once before any commands; read-only afterwards. No concurrent writes to race against | **no** |
| `WatchedKeys` | `watch.Manager.Watch` line 31 (under `m.mu`), `ClearWatch` from `watch.Manager.Unwatch` line 47 (under `m.mu`) | `watch.Manager.Unwatch` line 39 (under `m.mu`) | All access under `watch.Manager.mu` | **no** |
| `watchDirty` | `watch.Manager.NotifyMutation/NotifyAll` (engine, under `cache.Lock` and `m.mu`) | `HandleExec` (engine) | **Cross-goroutine: engine writes from one connection's command, another connection's `HandleExec` reads.** | **was YES, fixed in #32** via `atomic.Bool` |

### Subtle case: `evaluateInternal` is on the connection goroutine

The instrumentation block in `evaluateInternal` (lines 230, 231, 250,
307) reads `ctx.RexMeta` and `ctx.CmdMeta`. This block runs in the
**connection goroutine** (NOT inside a handler closure passed to the
engine), so it does not enter the engine. CmdMeta is also written in
the connection goroutine (`server.go:350/352`). RexMeta is written in
the engine goroutine — but the connection goroutine reads it AFTER
`evaluator.Evaluate` returns from a previous REX.META command, so the
`resChan` recv establishes happens-before.

### Subtle case: `routeToPlugin` reads RexMeta on the connection goroutine

`routeToPlugin` is called from `evaluateInternal:144` when the command
is plugin-routed. It reads `client.RexMeta` and `client.CmdMeta` on
the connection goroutine, sends to plugin via IPC, blocks for response.

A concurrent REX.META command from the SAME client cannot race because
the connection processes commands serially per-connection. A concurrent
mutation from ANOTHER client cannot race because each client has its
own `ClientContext`. **No race.**

## Conclusion

**No new races found beyond `watchDirty` (already fixed in #32).**

The audit confirms the architectural separation is sound:

- Per-connection `ClientContext` fields are safe because each connection
  has its own context and processes commands serially through the
  engine queue. The `cmdChan`/`resChan` round-trip per command provides
  the synchronization edge.
- The single field that crosses connections (`watchDirty`, written by
  another connection's mutation goroutine) is now atomic.
- `WatchedKeys` crosses connections via the watch manager but is gated
  by the manager's mutex, so writes and reads are serialised.

## Test coverage

`pkg/server/it_concurrency_test.go::TestIT_WatchPropagation_ReadLockBypass`
(added in PR #32) exercises the `watchDirty` cross-connection path
under `-race` for 500 ms. It passes consistently — the race detector
is what would surface a regression here.

`pkg/server/it_concurrency_test.go::TestIT_FlushDBPropagation`
(added in this audit) exercises the `NotifyAll` path: one connection
runs WATCH/MULTI/EXEC loops; another runs FLUSHDB to invalidate all
watched keys via `Manager.NotifyAll`. The same `atomic.Bool` carries
the cross-connection notification; this test is regression coverage
for the broader notification path.

## Methodology updates carried forward

The methodology lessons from #28 (mixed-workload baseline, cross-path
cost confirmation, magnitude-bounded risks) are documented in
`projects/gocache/plans/command-flow/diagnosis-pre-implementation.md`'s
"Lessons learned (post-#28)" section. They apply to any future plan
that changes synchronization primitives.

## Why no code changes ship in this PR

Methodology was added to the Obsidian plan note during the #32 closeout.
The audit found no new races. The only shippable artefacts are:

- This audit document (regression record for any future contributor
  asking "did anyone check?").
- The new `TestIT_FlushDBPropagation` stress test (regression coverage
  for the NotifyAll path).
- The Obsidian session note (audit chronicle).

The acceptance criterion "the next plan that changes synchronization
primitives cites this issue and includes the mixed-workload step" is
forward-looking — fulfilled by future plans, not by this PR.
