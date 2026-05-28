---
title: ADR-0022 Modular Performance Budget
description: Set benchmark gates and optimization order for IPC plugins, runtime instrumentation, lifecycle OTLP, and Pub/Sub fanout
status: accepted
date: 2026-05-28
deciders: [witherxse]
related:
  - 0006-builtin-vs-third-party-transport
  - 0014-pipelined-performance-limits
  - 0020-client-push-via-gcpc
  - Performance
---

# ADR-0022: Modular Performance Budget

## Context

GoCache's thesis is that safe extensibility and high performance can coexist. ADR-0006 keeps process-isolated IPC plugins for crash isolation and language flexibility, while ADR-0020 adds `ClientPushV1` so IPC plugins can implement Redis-compatible push behavior such as Pub/Sub.

The first end-to-end IPC and Pub/Sub benchmark captures show that the current modular path is functionally correct but too expensive for the thesis bar:

- Core GoCache vs `gocache-ipc`: request/response IPC is roughly 53-64% slower on the standard suite and 82-97% slower on the pipelined suite.
- Historical `gocache-ipc` vs `gocache-ipc-otel`: OTLP export added roughly 12-23% standard overhead and 17-22% pipelined overhead on top of the IPC path.
- Valkey vs `gocache-pubsub`: `PUBLISH_fanout_0` is 42,718 rps vs 15,814 rps (-63.0%), `PUBLISH_fanout_1` is 17,854 rps vs 9,836 rps (-44.9%), and `PUBLISH_fanout_10` is 2,060 rps vs 1,646 rps (-20.1%). Delivery correctness passed.

These deltas are larger than the accepted architectural budget for a modular microkernel. They should be treated as regressions to fix, not as structural costs like the Go mutex/network limits recorded in ADR-0014.

## Decision

Modular hot-path features must stay within a **20% throughput regression budget** against their fair baseline before they can be called performance-acceptable. For request/response observability, compare core GoCache and `gocache-ipc`; future runtime OTLP instrumentation gets its own benchmark target when the `instrumentation` plugin exists. For Redis-compatible plugin behavior such as Pub/Sub, compare against Valkey and preserve delivery correctness.

We keep custom GCPC over Unix domain sockets for the local hot path and optimize it before introducing heavier RPC or messaging frameworks. The optimization order is:

1. Add a per-plugin bounded FIFO writer loop in the router/connection layer so command hooks and operation hooks stop creating goroutines per send while preserving ordering and backpressure.
2. Split hook semantics into critical/blocking hooks and observe-only hooks; runtime instrumentation should use bounded async event delivery unless it explicitly needs denial/enrichment semantics; lifecycle OTLP remains embedded and startup-scoped.
3. Reduce GCPC allocation and correlation overhead with numeric request IDs and careful protobuf buffer reuse, only after benchmarks show the writer-loop and hook-mode changes are insufficient.
4. Batch or group `ClientPushV1` Pub/Sub fanout where possible. If fanout0/fanout1 remain outside the 20% budget after IPC batching, introduce a core or embedded Pub/Sub data-plane while keeping `ClientPushV1` as the generic extension primitive.

Prior negative experiments are binding constraints: do not reintroduce cache-wide read-lock bypass, do not blindly add long-lived buffer pools, and do not collapse embedded-vs-IPC transport choice into critical-vs-noncritical policy. Engine memory writes, atomic Redis semantics, and explicitly critical hooks may block; telemetry and push fanout should avoid blocking the command response path unless correctness requires it.

## Alternatives Considered

### Alternative 1: Accept current IPC/Pub/Sub overhead as the cost of isolation

- **Pros**: No additional code complexity; preserves the current implementation.
- **Cons**: Makes common modular features 45-65% slower in measured scenarios, which is too far from the thesis claim that safe extensibility can coexist with high performance.
- **Why not**: ADR-0014 already distinguishes structural safe-Go costs from fixable overhead. The current IPC/Pub/Sub deltas are in the fixable category because they include goroutine-per-send, blocking observability hooks, and unbatched push fanout.

### Alternative 2: Replace GCPC with gRPC, Connect, NATS, or Watermill

- **Pros**: Mature ecosystems, richer tooling, built-in flow-control patterns.
- **Cons**: Adds dependencies, more framing/state machinery, and usually a larger local hot-path cost than the current UDS/protobuf transport. Some options also change the deployment model from local extension IPC to distributed messaging.
- **Why not**: The immediate bottleneck is not lack of RPC features; it is local scheduling, blocking, allocation, and fanout shape. Heavy frameworks remain useful for future distributed integrations, not for the local plugin hot path.

### Alternative 3: Move every hot plugin into core or embedded mode

- **Pros**: Lowest overhead for observability and Pub/Sub.
- **Cons**: Removes crash isolation and runtime deploy independence for all hot features, undermining the microkernel architecture.
- **Why not**: Embedded mode is an escape hatch for startup-critical or budget-failing data planes, not a replacement for IPC plugins. The first pass should make IPC fast enough for noncritical extensions.

### Alternative 4: Retry broad engine-level lock bypasses or buffer pools

- **Pros**: Could theoretically reduce core overhead across all commands.
- **Cons**: Prior experiments showed cache-wide read-lock bypass and generic buffer pooling regressed throughput/memory or added complexity without a durable win.
- **Why not**: The current regression is specifically in IPC/hook/push mechanics. Repeating rejected core experiments distracts from the measured hot path.

## Consequences

### Positive

- Provides a hard benchmark gate for modular architecture claims.
- Keeps ADR-0006's IPC isolation model while making performance regressions actionable.
- Preserves ADR-0020's generic push primitive but prevents the first direct-write implementation from becoming the final performance story by accident.
- Gives every optimization phase a before/after measurement requirement.

### Negative

- Adds benchmarking work to every modular performance change.
- Requires more nuanced hook policies: critical hooks remain synchronous, observe-only hooks use bounded async delivery.
- Pub/Sub may need a specialized data-plane if generic `ClientPushV1` batching cannot hit the budget.

### Risks

- **Risk**: The 20% budget is too strict for some third-party integrations. **Mitigation**: Treat it as the gate for built-in thesis claims and hot-path features; third-party connectors can document a different deployment-specific budget.
- **Risk**: Async observe-only hooks drop telemetry during overload. **Mitigation**: Expose dropped-event counters and keep critical hooks on the blocking path.
- **Risk**: Batching improves throughput but increases latency. **Mitigation**: Re-run both rps and p99 comparisons; reject batching that meets throughput while violating latency or delivery correctness.
