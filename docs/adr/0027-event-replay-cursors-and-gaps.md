---
title: ADR-0027 Event Replay, Cursors, and Gaps
description: Split lifecycle/control replay from high-rate runtime telemetry and make gaps explicit
status: proposed
date: 2026-05-29
deciders: [witherxse]
related:
  - 0024-async-event-delivery-and-command-reaction-points
  - 0026-event-traffic-classes-and-backpressure
  - 0023-lifecycle-otlp-and-runtime-instrumentation
---

# ADR-0027: Event Replay, Cursors, and Gaps

## Context

GoCache already uses a bounded event replay ring so late subscribers can see important lifecycle and operation context. The async event IPC plan adds interest masks, bounded best-effort queues, sampling, and batching for high-rate runtime telemetry. Those mechanisms mean some high-rate events may never be produced, may be dropped, or may only be available as sampled records.

A late subscriber must not be given a misleading partial history that looks complete. Replay policy needs to distinguish low-volume lifecycle/control history from high-rate runtime telemetry.

## Decision

Lifecycle and control events remain candidates for bounded eager replay because they are low-volume and important for startup/failure context.

High-rate runtime telemetry events use explicit cursor, sequence, and gap semantics rather than guaranteed full replay. If high-rate events are skipped due to interest masks, dropped due to queue overflow, sampled, or overwritten in a bounded ring, the system exposes that fact through gap markers and counters. A subscriber may opt into runtime replay/cursor behavior only under the delivery modes and retention limits documented by the subscription API.

Replay guarantees are per event class, not globally uniform. The event contract must state whether each class is eager-replayable, cursor-replayable, live-only, sampled, or best-effort.

## Alternatives Considered

### Alternative 1: Replay every event class from one bounded ring

- **Pros**: Simple subscriber model.
- **Cons**: High-rate command telemetry can evict lifecycle history and create huge replay bursts.
- **Why not**: Runtime telemetry volume and lifecycle/control semantics are different enough to require separate policy.

### Alternative 2: Disable replay entirely

- **Pros**: Simplest producer hot path and no replay confusion.
- **Cons**: Loses startup context and late-subscriber observability that prior work intentionally added.
- **Why not**: Lifecycle replay is valuable and low-volume; the problem is high-rate runtime telemetry, not replay itself.

### Alternative 3: Persist all event history durably

- **Pros**: Strongest replay story.
- **Cons**: Turns the cache server into an event store and adds disk/I/O policy outside this thesis slice.
- **Why not**: GoCache needs bounded operational replay, not durable telemetry storage.

### Alternative 4: Hide gaps behind counters only

- **Pros**: Reduces event stream noise.
- **Cons**: Subscribers consuming replay/cursor streams may not know where their local view became incomplete.
- **Why not**: Gaps must be visible at the point where a consumer is reconstructing history, not only in aggregate metrics.

## Consequences

### Positive

- Keeps startup/lifecycle replay useful without over-promising runtime telemetry history.
- Makes interest masks compatible with replay: events not produced are not later pretended to exist.
- Gives plugins a path to detect incomplete history and degrade gracefully.
- Aligns with bounded queue/drop accounting from ADR-0026.

### Negative

- Adds public semantics around event classes, cursors, and gaps that must be maintained.
- Plugin SDKs must teach authors how to handle live-only, replayable, and gap-marked streams.
- Tests need to cover late subscription, overflow, skipped production, and gap visibility.

### Risks

- **Risk**: Cursor semantics become too expensive on the command hot path. **Mitigation**: Use cheap sequence counters and keep cursor metadata scoped to classes that need it.
- **Risk**: Global ordering is assumed where only per-class ordering exists. **Mitigation**: Document ordering guarantees per event class and avoid claiming total global replay ordering unless implemented.
- **Risk**: Gap events themselves flood subscribers during overload. **Mitigation**: Coalesce gaps by subscriber/event class and expose aggregate counters.
