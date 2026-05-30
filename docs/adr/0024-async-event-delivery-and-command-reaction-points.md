---
title: ADR-0024 Async Event Delivery and Command Reaction Points
description: Make server events asynchronous observational notifications and reserve synchronous command behavior for explicit reaction hooks
status: proposed
date: 2026-05-29
deciders: [witherxse]
related:
  - 0022-modular-performance-budget
  - 0023-lifecycle-otlp-and-runtime-instrumentation
  - Performance
  - Plugins
---

# ADR-0024: Async Event Delivery and Command Reaction Points

## Context

GoCache's runtime observability path was moved away from synchronous operation hooks toward typed server events so IPC plugins can observe command activity without blocking Redis command execution. The first event-only Prometheus measurements improved standard workloads, but pipelined IPC Prometheus still exceeds the modular performance budget. Analysis found that the current event path is only asynchronous after the plugin bridge enqueue: event subscribers still run from the emitter goroutine, and the manager bridge currently pays clone/filter/envelope/enqueue cost before command evaluation returns.

At the same time, some plugin integration points legitimately need synchronous behavior. Command admission, denial, enrichment, or response-order-sensitive behavior must remain on explicit hook surfaces; asynchronous events must not become a hidden way to affect command outcomes.

## Decision

Server-produced events are asynchronous, at-most-once, observational notifications. They do not deny commands, enrich command context, change command results, reorder RESP replies, or participate in Redis transaction semantics.

Synchronous command behavior remains limited to explicitly declared command reaction points: command pre-hooks and command post-hooks when the plugin must affect admission, denial, enrichment, response semantics, or ordering. Runtime telemetry and metrics use asynchronous event delivery by default.

RESP pipelining remains N commands with N ordered replies. Event batching may group telemetry transport records, but it does not collapse command execution, errors, responses, transactions, auth decisions, persistence ordering, or hook semantics.

## Alternatives Considered

### Alternative 1: Keep command telemetry on synchronous hooks

- **Pros**: Simple mental model; every plugin sees command activity inline and can rely on command order.
- **Cons**: Makes observability plugins part of the command latency path and violates the <=20% modular performance budget under pipelining.
- **Why not**: Telemetry is observational. It should not block normal command execution unless a plugin explicitly registered a reaction hook.

### Alternative 2: Make Prometheus aggregation a core special case

- **Pros**: Lowest overhead for the Prometheus benchmark path.
- **Cons**: Couples one plugin's aggregation model into the server and undermines the microkernel principle that plugins choose how to consume events.
- **Why not**: The problem is generic event delivery overhead, not Prometheus semantics. Core must provide better generic event delivery surfaces.

### Alternative 3: Treat a RESP pipeline as one logical operation

- **Pros**: Would greatly reduce per-command telemetry records under pipelined benchmarks.
- **Cons**: Breaks Redis semantics: each pipelined command has its own result, error, mutation, authorization, and ordering constraints.
- **Why not**: Pipelining is network batching only. Telemetry transport may batch records, but command semantics remain per command.

### Alternative 4: Make every event lossless and blocking

- **Pros**: Strongest delivery guarantee for plugins.
- **Cons**: Reintroduces plugin backpressure into the command path and lets slow telemetry consumers degrade cache throughput.
- **Why not**: Lossless synchronous behavior belongs on reaction hooks, not observational events. Event loss must be visible, not silently prevented by blocking commands.

## Consequences

### Positive

- Clarifies that async events are not a backdoor for command mutation or denial.
- Preserves plugin freedom: plugins can request raw, batched, sampled, or replayable event delivery modes without forcing that shape on core.
- Makes pipelined event batching safe to pursue as a transport optimization.
- Keeps the performance plan aligned with ADR-0022 and ADR-0023.

### Negative

- Plugins that previously used hook-like behavior for telemetry must choose the right surface: reaction hook for command influence, event subscription for observation.
- Event delivery needs explicit drop, lag, and replay semantics because it is not inherently lossless.
- The event bus/bridge may require more machinery than the current simple subscriber callback model.

### Risks

- **Risk**: A plugin author expects event handlers to affect command flow. **Mitigation**: SDK docs and GCPC contracts must describe events as observational and direct plugin authors to command hooks for reaction behavior.
- **Risk**: Async event drops hide important operational data. **Mitigation**: ADR-0026 defines traffic classes, backpressure, and drop accounting; ADR-0027 defines replay/cursor/gap behavior.
- **Risk**: Event batching accidentally changes pipeline semantics. **Mitigation**: Tests must assert one command result per pipelined command and one event record per observed command inside any batch.
