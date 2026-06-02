---
title: ADR-0029 OperationTracker Sidecar for Low-Overhead Telemetry
description: Move operation telemetry production off the command goroutine through a low-allocation OperationTracker submit path and worker-owned telemetry projection
status: proposed
date: 2026-06-01
deciders: [witherxse]
related:
  - 0022-modular-performance-budget
  - 0023-lifecycle-otlp-and-runtime-instrumentation
  - 0024-async-event-delivery-and-command-reaction-points
  - 0026-event-traffic-classes-and-backpressure
  - 0027-event-replay-cursors-and-gaps
  - 0028-operation-observability-and-log-records
  - 0030-gcpc-v1-observability-context-contract
  - 0031-operation-identity-export-contract
  - 0032-telemetry-context-version-ownership
  - 0033-common-operationtracker-sharding-contract
  - projects/gocache/research/gocache-zero-allocation-telemetry-sidecar-implementation-brief.md
---

# ADR-0029: OperationTracker Sidecar for Low-Overhead Telemetry

## Context

Runtime observability is a product requirement for GoCache, but the current instrumentation path breaks the modular performance budget from ADR-0022. The current command path can still build operation context snapshots, typed event payloads, event-bus fanout records, plugin bridge projections, and GCPC envelopes before the command goroutine returns. The 2026-05-31 runtime OTLP evidence in `bench/results/runtime-otlp-benchstats-20260531/` shows large throughput, p99, allocation, mutex-wait, and queue-pressure regressions when full runtime instrumentation is enabled.

The reference research note `projects/gocache/research/gocache-zero-allocation-telemetry-sidecar-implementation-brief.md` proposes a concrete zero-allocation submit model: pointer-free records, per-producer rings, worker-owned operation replay/projection, connection context version references, and off-hot-path event/protobuf materialization. This ADR accepts that direction as the performance-first architecture. The goal is not to create a broad observability platform, telemetry schema, or server-side aggregation layer first; event-model and GCPC changes are supporting work needed to remove expensive telemetry materialization from Redis command execution.

This ADR is intentionally the umbrella performance decision. Focused follow-up ADRs define the GCPC typed-event boundary (ADR-0030), operation identity strategy boundary (ADR-0031), telemetry context version ownership (ADR-0032), and the common/sharded OperationTracker placement (ADR-0033). Connection id formatting and a broader event-bus decoupling are acknowledged but intentionally out of scope for this ADR set unless the user reopens them.

## Decision

GoCache introduces an `OperationTracker` sidecar pattern for runtime telemetry. The command goroutine submits compact, pointer-free operation records into a bounded tracker path and returns to command execution; worker goroutines own the expensive work of telemetry-context folding, typed event/log projection, and GCPC/protobuf materialization. Workers must not turn this into server-owned semantic grouping, server-owned aggregation, or exporter-specific telemetry calculation.

The public package for plugin-safe observability contracts is `api/observability`, and it contains interfaces plus stable value contracts only. Reusable concrete tracker, ring, sharding, and operation-identity strategy primitives that both the server and plugin tooling can share live under `commons/observability`. Concrete server wiring, connection context version stores, worker scheduling, subscriber queues, and projection internals remain server-owned implementation under `pkg/`. Plugins interact through SDK/API facades, common primitives where appropriate, and GCPC messages; they do not import `pkg/operations`, `pkg/events`, `pkg/plugin`, `pkg/workers`, or any server-internal tracker implementation.

The sidecar design separates telemetry context from Go `context.Context`. Go `context.Context` remains in APIs that need cancellation, deadlines, or request lifetime. Telemetry context becomes operation-associated data managed by `OperationTracker` and carried as immutable snapshots, context patches, or typed event/log fields depending on the boundary. Connection context version references are server-only: the server captures the connection-context version current at operation start, while IPC plugins receive serialized filtered context or provide their own producer context when they create plugin-owned operations.

Server-produced runtime telemetry is streamed or exposed as generic operation/event/log records for interested plugins. The server does not calculate grouped telemetry, aggregates, traces, dashboards, exporter-specific summaries, or backend-shaped records. Aggregation, export, sampling, rollups, and backend-specific optimization are plugin responsibilities. Metrics work that is currently synchronous on the command goroutine may later adopt a tracker-style raw-observation submit path, but server-owned metrics aggregation is not part of this ADR.

GCPC v1 is intentionally reshaped in place to support this model, as described by ADR-0030. Backward compatibility is explicitly out of scope: GoCache is a single-user development/thesis system at this stage, not a production public protocol, so the implementation may break existing plugin/event contracts to reach the cleaner low-overhead model.

## Contract Shape

### OperationTracker submit path

The target hot path is:

1. create or receive an operation id from the configured operation-identity strategy;
2. submit operation start, event, log, context-patch, command-completion, and operation-finish records as value records;
3. avoid protobuf construction, maps, string formatting, closures, `interface{}` values, and subscriber-specific projection on the command goroutine;
4. preserve per-operation ordering for records that reach the tracker through FIFO submission and worker-side replay;
5. project public typed events, log records, and GCPC payloads off the command goroutine without calculating semantic groups or aggregates in the server.

The exact ring implementation, worker sharding, record layout, and allocation strategy are implementation details, but the accepted performance contract is clear: the submit path must be benchmarked and defended as zero or near-zero allocation, and any per-command allocation on the serving goroutine is treated as a regression unless explicitly justified.

### Public observability facade

`api/observability` should expose stable concepts such as operation identifiers, operation references, event records, log records, context patches, and plugin-safe producer/recorder interfaces. SDK helpers should expose ergonomic handles for plugin-owned background work and may use a `TelemetrySupportingPlugin`-style capability so telemetry-capable IPC plugins receive the server-selected operation identity strategy and producer helpers during registration.

The public facade may use names such as `OperationTracker`, `Recorder`, `OperationHandle`, `Worker`, or `TelemetrySupportingPlugin`, but those names describe capabilities. They must not expose the concrete server registry, connection context version table, event bus, worker goroutines, replay ring, or queue implementation.

### Operation ownership and identity configuration

Server-started work uses server-created operations. When a plugin reacts to server-caused work, it may start a plugin-owned child operation with the source server operation id as parent id when that causal relationship is real. Plugin-originated background work produces its own root or child operation through the SDK/API facade.

The operation identity contract is split out to ADR-0031. This ADR only requires the sidecar to keep optimized internal handles separate from exported operation identifiers and to use the server-selected identity strategy consistently. The GCPC server injects the selected identity strategy/configuration into IPC plugins at registration, while embedded plugins receive the already configured server `OperationTracker`. Current prefixed counters such as `cmd_1` are not sufficient for plugin self-assignment without extra coordination.

### Ordering and fire-and-forget delivery

Ordering is operation-centric. Records for one operation must replay in submit order when they are accepted by the tracker. Global total ordering across all connections and plugins is not part of this ADR. Parent-child relationships are expressed through operation ids and parent ids; cross-operation ordering beyond that is reconstructed from timestamps, sequence fields, and parent relationships.

Telemetry is fire-and-forget and may drop under overload. It is not an audit log and does not need durable buffering in this ADR set. The first implementation should prefer non-blocking command execution over backpressure: if bounded rings, worker queues, or plugin delivery paths cannot keep up, records may be dropped. The implementation should expose cheap overload/drop visibility through counters, health/query surfaces, or gap metadata where practical, but drop policy is no longer a gate that blocks the sidecar design.

Warning/error logs and lifecycle records may receive priority in implementation, but they remain telemetry signals unless a later durable/audit event system explicitly reclassifies them.

## Priority Review Gates Before Implementation

This ADR remains `proposed` until the following user-visible gates are resolved or explicitly deferred:

1. **Operation identity wiring**: define how the server-selected UUIDv7 or W3C strategy is exposed through API/commons, injected into IPC plugins through GCPC registration/configuration, and reused by embedded plugins.
2. **GCPC typed-event projection**: align the worker projection target with ADR-0030's typed oneof events plus common context/source fields.
3. **Context snapshot/version proof**: align with ADR-0032 so command-time context is event-time correct and GCPC receives serialized/dereferenced context, not server version ids.
4. **Performance proof**: require package benchmarks plus Docker benchmark before/after evidence for submit-path allocation, p99 latency, overload visibility, and IPC projection cost before accepting the implementation.
5. **Common/API split**: keep `api/observability` as interfaces/value contracts, `commons/observability` as reusable engine primitives and strategy implementations, and server projection/context-version state in `pkg/` as described by ADR-0033.

## Alternatives Considered

### Alternative 1: Keep the current synchronous typed event path

- **Pros**: Smallest conceptual change and preserves current `EventV1` oneof payload consumers.
- **Cons**: Leaves operation snapshots, event construction, bus fanout, projection, and enqueue work on the command goroutine.
- **Why not**: This is the path that produced the runtime OTLP regression evidence. It cannot satisfy full runtime observability and ADR-0022's modular performance budget at the same time.

### Alternative 2: Optimize only GCPC batching and protobuf serialization

- **Pros**: Lower blast radius than changing operation/event production.
- **Cons**: The command goroutine would still pay producer-side operation/context/event costs before batching helps.
- **Why not**: Existing measurements show producer-side work dominates enough that downstream IPC batching alone is not the first fix.

### Alternative 3: Introduce GCPC v2 or a rich telemetry schema

- **Pros**: Clean versioned migration surface and an opportunity to model traces, logs, metrics, and aggregates explicitly.
- **Cons**: Doubles the protocol surface, makes the server define a telemetry product model, and postpones the performance fix behind compatibility scaffolding.
- **Why not**: GoCache must stay generic by default for maximum extensibility. The accepted direction is to rewrite existing `gcpcv1` in place around typed observability records with common fields and let plugins define any richer telemetry schema or aggregate model they need.

### Alternative 4: Expose the concrete server tracker to plugins

- **Pros**: Gives plugins maximum control and avoids designing a separate facade.
- **Cons**: Breaks plugin isolation, leaks server internals, exposes connection-context versioning to plugins that do not own client connections, and makes future tracker rewrites public breakage.
- **Why not**: Plugins need stable operation producer capabilities, not the server's mutable registry and worker internals.

### Alternative 5: Put grouping and aggregation in the server

- **Pros**: Centralizes metrics, traces, logs, event summaries, and precomputed views.
- **Cons**: Turns core into an observability backend, couples plugin-specific export semantics into the server, and reduces the generic extension surface.
- **Why not**: Explicitly rejected. GoCache's microkernel model keeps grouping, aggregation, calculation, export, and optimization in plugins. The server streams generic operation/event/log records and protects the command path.

### Alternative 6: Make telemetry durable/audit-grade now

- **Pros**: Stronger completeness guarantees and easier forensic replay.
- **Cons**: Requires WAL, retention, replay, compaction, and failure semantics that are much larger than the telemetry performance fix.
- **Why not**: Telemetry itself is not audit-durable in this ADR set. Durable/audit event handling is future work after the telemetry/event split is clearer.

## Consequences

### Positive

- Moves telemetry materialization, protobuf construction, and subscriber projection off the command goroutine.
- Gives GoCache a single operation-centric observability model that can apply to commands, server lifecycle, workers, plugin lifecycle, and plugin-owned asynchronous work.
- Keeps plugin isolation: public contracts live in `api/observability`, reusable implementation primitives live in `commons/observability`, and server internals stay in `pkg/`.
- Makes future metrics, tracing, and logging plugins consume the same operation/event/log record stream while keeping all aggregate calculation and backend shaping plugin-owned.
- Creates a direct benchmark target: submit path allocations, command p99, mutex/block profiles, GC frequency, queue drops/gaps, and downstream projection cost.

### Negative

- Requires broad refactoring across pipeline, events, GCPC, plugin manager, SDK, instrumentation, tests, and docs.
- Current `EventV1` consumers must be rewritten for the normalized typed oneof/common-field contract in ADR-0030.
- Strategy wiring, GCPC registration/configuration, context materialization, and public facade naming must be fixed before implementation is considered complete.
- Worker-side projection moves CPU rather than deleting it; total throughput still depends on raw-record projection cost, transport batching, and worker scheduling.

### Risks

- **Risk**: The sidecar becomes a product/platform redesign instead of a performance fix. **Mitigation**: Scope the first ADR and implementation around operation telemetry performance; explicitly keep grouped telemetry, aggregation, rich telemetry schemas, durable audit semantics, and backend-specific calculation out of the server.
- **Risk**: The submit path is advertised as zero-allocation but gains hidden allocations through strings, closures, interfaces, or escaping variadics. **Mitigation**: Require `go test -bench -benchmem`, escape-analysis checks for hot-path APIs, and regression tests around submit-path allocation counts.
- **Risk**: Worker overload silently hides telemetry loss. **Mitigation**: Treat telemetry as drop-allowed but expose cheap drop counters, gap metadata, or health/query signals so missing telemetry is not misread as complete telemetry.
- **Risk**: Plugins receive server-only connection context version details. **Mitigation**: Keep connection context version ids in `pkg/`; plugins receive serialized filtered context through `api/observability`/GCPC.
- **Risk**: Operation ids collide once plugins self-assign child operations. **Mitigation**: Use the server-selected UUIDv7 or W3C strategy from ADR-0031 and inject that strategy/configuration into plugin tooling instead of preserving legacy prefixed counters.
- **Risk**: Removing telemetry context maps is confused with removing Go `context.Context`. **Mitigation**: Keep Go `context.Context` for cancellation/deadline and document telemetry context as a separate operation-associated data model.
