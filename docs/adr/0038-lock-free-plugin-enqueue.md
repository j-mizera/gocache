---
title: ADR-0038 Lock-free plugin enqueue
description: Replace PluginConn's sendMu-guarded enqueue path with atomic closed state and done-channel shutdown detection.
status: accepted
date: 2026-07-10
deciders: [GoCache maintainers]
related:
  - docs/performance/README.md
  - pkg/plugin/router/router.go
---

# ADR-0038: Lock-free plugin enqueue

## Status

Accepted — 2026-07-10

## Context

The `PluginConn.enqueue` function serialized ALL concurrent senders through a single `sendMu` mutex, even though Go channels are inherently goroutine-safe. The mutex was held during the channel send (`pc.outbound <- item`), forcing N concurrent goroutines to enqueue one-at-a-time. The FR-004 concurrent throughput benchmark identified this as the primary serialization bottleneck.

The mutex's sole semantic role was atomicity of the `closed` check + enqueue: ensuring no item could be enqueued after `Close()` set `closed = true`. This can be achieved without a mutex using `atomic.Bool` for the fast-path check and the `done` channel in the select for happens-before-correct Close() race detection.

## Decision

Replace `sendMu sync.Mutex` + `closed bool` with `closed atomic.Bool` + a `case <-pc.done:` branch in the enqueue select. Remove the mutex entirely.

- **Fast path**: `closed.Load()` is a single atomic read — cheaper than mutex Lock/Unlock, no contention.
- **Close race**: if `Close()` runs between the `Load()` check and the select, the `case <-pc.done:` branch catches it and returns `ErrPluginDown`.
- **Close() ordering**: `closed.Store(true)` → `close(pc.done)` → `conn.Close()` → wait `writerDone` → `drainPending()`. The `closeOnce.Do()` provides once-only guarantee.

## Alternatives Considered

### Alternative 1: Keep `sendMu`

- **Pros**: Preserves the previous strict "never enqueue after close" guarantee and requires no semantic documentation.
- **Cons**: Keeps all concurrent senders serialized behind one mutex even though the outbound channel is goroutine-safe.
- **Why not**: It leaves the FR-004 benchmark's primary enqueue serialization bottleneck in place.

### Alternative 2: Close the outbound channel

- **Pros**: Could make shutdown visibly reject future sends through the channel itself.
- **Cons**: Concurrent sends racing with channel close can panic unless another synchronization mechanism is added.
- **Why not**: Avoiding send-on-closed-channel panics would reintroduce a guard or require a larger writeLoop/outbound-channel redesign outside this decision.

## Consequences

### Semantic shift (documented)

The old code guaranteed "never enqueue after close" (mutex made closed-check + enqueue atomic). The new code allows a benign race: an item may be enqueued after `Close()` has started but before `writeLoop` exits. This is safe because:

1. If `writeLoop` is still draining: the item is processed, the transport write fails on the closed connection, and the error is delivered via `errCh`. The blocking `Send` caller receives the error.
2. If `writeLoop` already exited: the item sits in the `outbound` channel unread. For fire-and-forget sends, this is an acceptable drop. For blocking sends, `Send`'s own `case <-pc.done:` branch returns `ErrPluginDown`. No goroutine leak.

The caller ALWAYS receives an error for blocking sends. Fire-and-forget drops during shutdown are acceptable.

### Metrics change

`enqueueLatency` previously included mutex contention wait time. Post-change, it measures only the atomic-load + select cost. This is a baseline-breaking change for longitudinal benchmarks — historical numbers are not comparable.

### Stats accuracy during shutdown

`sendAccepted` may over-count for items enqueued during the Close() race window that are effectively dropped. This is minor, shutdown-only, and not a correctness bug.

### Positive

- Concurrent senders no longer serialize on `sendMu` before reaching the outbound channel.
- Shutdown detection remains explicit through `atomic.Bool` and `done`.

### Negative

- The strict closed-check + enqueue atomicity guarantee is intentionally relaxed during shutdown.
- Enqueue-latency time series before and after this ADR are not directly comparable.

### Risks

- **Risk**: Shutdown-only `sendAccepted` counts can include items that are never written. **Mitigation**: Treat these counters as enqueue-path telemetry and document the shutdown caveat.
- **Risk**: Future changes could reorder `Close()` and weaken race handling. **Mitigation**: Preserve the documented `closed.Store(true)` → `close(pc.done)` → `conn.Close()` → `writerDone` → `drainPending()` ordering.

## Verification

- `go test -race ./pkg/plugin/router/` — race detector clean
- `TestPluginConnConcurrentSendAndCloseRace` — N goroutines + parallel Close stress test
- BenchmarkConcurrentCommandThroughput_Shared — throughput improvement at goroutine counts >= 4
