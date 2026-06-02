---
title: ADR-0030 GCPC v1 Observability Context Contract
description: Rewrite existing GCPC v1 observability messages in place so typed events, logs, and reactions share operation correlation, source, and serialized context
status: proposed
date: 2026-06-01
deciders: [witherxse]
related:
  - 0023-lifecycle-otlp-and-runtime-instrumentation
  - 0024-async-event-delivery-and-command-reaction-points
  - 0026-event-traffic-classes-and-backpressure
  - 0027-event-replay-cursors-and-gaps
  - 0028-operation-observability-and-log-records
  - 0029-operationtracker-sidecar-low-overhead-telemetry
  - 0031-operation-identity-export-contract
  - 0032-telemetry-context-version-ownership
  - 0033-common-operationtracker-sharding-contract
  - projects/gocache/research/gocache-zero-allocation-telemetry-sidecar-implementation-brief.md
---

# ADR-0030: GCPC v1 Observability Context Contract

## Context

GCPC v1 currently mixes several observability shapes. `CommandRequestV1`, `HookRequestV1`, and `OperationHookRequestV1` carry context maps; `EventV1` has a top-level `operation_id` but its typed oneof payloads carry inconsistent detail; `RuntimeLogRecordV1` carries `operation_id`, `level`, `source`, and `fields`; connection, plugin, cache, auth, server, and replay events do not all carry the same operation context surface. This inconsistency makes plugin authors reason about each GCPC structure separately and encourages hot-path code to materialize typed payloads too early.

ADR-0029 accepts a performance-first `OperationTracker` sidecar that submits compact records on the command path and materializes public records from worker-owned state. That design needs a consistent GCPC observability boundary: records should be generic in the system sense, so plugins can calculate, aggregate, optimize, and export however they choose, while still preserving generated typed event payloads where the event has known fields. The server streams records and enforces visibility; it must not become a telemetry schema owner, grouped-telemetry calculator, or aggregator.

This ADR intentionally rewrites the existing `api/gcpc/v1/gcpc.proto` contract in place. There is no GCPC v2, no side-by-side compatibility bridge, and no backward-compatibility requirement for this thesis branch. GoCache is still a single-user development system, so breaking existing plugin/event contracts is acceptable when it removes legacy shape and supports the OperationTracker performance refactor. `task proto` and the generated `api/gcpc/v1/gcpc.pb.go` become the first implementation checkpoint.

Operation identity details are governed by ADR-0031, server-side telemetry context version ownership by ADR-0032, and common/sharded tracker placement by ADR-0033. Connection id formatting and broad event-bus decoupling are not active decisions in this ADR.

## Decision

GCPC v1 observability messages are normalized around operation-correlated typed records. The current typed `EventV1` oneof taxonomy remains the event payload model, but each event family receives a common observability surface for routing, correlation, source/provenance, and serialized filtered context. The goal is not a fully generic untyped data shape; the goal is a generic system contract that keeps event-specific fields typed.

Every GCPC structure that represents an operation, event, log, hook reaction, plugin command execution, worker action, or emitted observability record must either carry an explicit operation reference and filtered serialized context or be documented as a non-observability transport/control frame. The exact protobuf field names are implementation details, but the common surface must preserve these concepts:

- unique event/record id where the record is independently replayable or deduplicable;
- event type/name used for routing and subscription matching;
- timestamp of occurrence;
- operation id;
- parent operation id when a real parent is available;
- source/provenance, including plugin name when produced by a plugin;
- filtered serialized telemetry context;
- typed oneof payload fields for event-specific data.

General event severity is not part of this event contract. Logs may have log severity/level; non-log event priority, delivery class, or audit durability are not modeled as first-slice GCPC filter dimensions.

`EventSubscribeV1` remains the subscription entry point, but it filters by event type only in the first implementation. Source/provenance remains a common field for loop prevention, diagnostics, and later filtering, but server-side filtering beyond event type is explicitly deferred until measurements or a later ADR justify it. Server-side grouping and aggregation are not part of this contract. Plugins receive typed records and decide whether to build traces, logs, metrics, audits, dashboards, rollups, or custom aggregates.

Plugin-visible context is serialized and dereferenced before it crosses GCPC. Server-only connection context version ids never cross GCPC, and any implementation that exposes them in protobuf or SDK types violates this ADR.

## Contract Shape

### Typed event records with common fields

`EventV1` keeps a generated oneof payload model for known event families. The common fields above sit beside the oneof payload so plugins can route and correlate records without decoding every payload, while still receiving compile-time typed event detail for known records.

The common event surface may be informed by generic envelope ideas from systems such as OpenTelemetry or CloudEvents, but GoCache does not adopt either as the server's telemetry schema and does not bake an exporter worldview into core. GoCache also does not use an unbounded `map[string]any`-style event body as its primary event data shape.

### Common context and source invariants

The implementation must define generated protobuf fields and tests for:

- required common fields for server-produced observability records;
- explicit ownership boundaries between common fields, serialized filtered context, and typed oneof payload fields;
- size/cardinality limits for context maps, payload fields, and any bounded extension data;
- redaction/filtering before any record crosses the plugin boundary;
- source/provenance fields sufficient to avoid plugin telemetry feedback loops;
- event-type subscription matching that does not require decoding plugin-owned payloads.

### Operation references and context

Any record that represents or describes work must carry operation correlation. Operation correlation is not optional for observability records unless a frame is explicitly transport-only. The normalized shape should support:

- server-created operations;
- plugin-created child operations that use a server operation id as parent when reacting to server-caused work;
- plugin-originated root operations for plugin background work;
- log records correlated with an operation;
- event records correlated with an operation;
- worker records correlated with the worker operation that emitted them.

Context is telemetry context, not Go `context.Context`. GCPC carries serialized filtered telemetry context produced from the server's event-time context model. Go `context.Context` is an in-process cancellation/deadline mechanism and is not serialized through GCPC.

### Plugin-originated telemetry

IPC plugins may emit plugin-owned operations, typed events, log records, and metric observations through the normalized GCPC v1 observability surface. These records carry plugin provenance and a self-assigned standardized operation id produced through the server-injected identity strategy from ADR-0031. When the plugin is reacting to a server event, hook, or command, it uses the server operation id as the parent id of its plugin-owned operation only when that relationship is real.

Instrumentation and exporter plugins must not receive their own telemetry by default. Self-observation is opt-in and must be marked explicitly to avoid feedback loops where an exporter logs its export and then exports that log again.

### Runtime logs

Runtime logs become log records or transport batches of log records that share the same operation/source/context surface as events, but logs keep log-specific fields such as level/severity, caller, message/body, and structured fields. Transport batching is allowed only as an IPC efficiency detail; it must not become semantic grouping or server-side log aggregation. Durable log buffering is deferred to a future decision.

### Operation hooks and plugin commands

Operation hooks, command hooks, and plugin command requests remain reaction surfaces, not passive observability streams. They should still carry operation ids and filtered context consistently. If they add or mutate telemetry context, they do so through explicit context values or patches that can be folded by `OperationTracker`, not by directly sharing mutable maps across goroutines or processes.

### Replay and gaps

Telemetry-dependent plugins may need replay or gap visibility, but this ADR does not claim to be the final durable event system. Replay/gap records use the same typed event/common-field shape when they are exposed through GCPC. Drop counters remain core health counters, not grouped telemetry products; when practical, they should be exposed outside saturated telemetry streams so plugins can decide how to aggregate or alert on them.

## Priority Review Gates Before Implementation

This ADR remains `proposed` until these GCPC-specific gates are resolved or explicitly deferred:

1. **Record vs control frame taxonomy**: enumerate which GCPC messages are observability records and which remain transport/control/reaction frames.
2. **Typed event common fields**: define the fixed common fields, typed oneof payload ownership, context representation, payload bounds, and required-field tests before editing generated protobuf consumers.
3. **Subscription filters**: keep the first subscription filter to event type only; document any source/provenance/delivery filters as future work unless later measurements reopen them.
4. **Operation identity fields**: align the protobuf shape with ADR-0031's UUIDv7/W3C strategy output and GCPC registration/configuration flow.
5. **Context version boundary**: ensure GCPC never exposes server context version ids; it only carries serialized filtered context as defined by ADR-0032.

## Alternatives Considered

### Alternative 1: Replace typed `EventV1` oneof payloads with a fully generic envelope

- **Pros**: One payload shape for every event and less generated protobuf schema churn.
- **Cons**: Loses compile-time event fields, encourages untyped payload maps, and makes plugin mistakes appear at runtime instead of schema review time.
- **Why not**: The user explicitly wants typed oneof event implementations with available fields. “Generic” means generic system ownership, not generic data shape.

### Alternative 2: Add GCPC v2 or a rich telemetry schema

- **Pros**: Cleaner compatibility story, less breakage for current tests, and room to model traces/logs/metrics/aggregates explicitly.
- **Cons**: Doubles protocol surface, makes the server own a telemetry product model, and delays the performance-motivated refactor behind compatibility scaffolding.
- **Why not**: GoCache must stay generic by default for maximum extensibility. The accepted thesis-branch direction is to reshape `gcpcv1` in place around typed records with common fields and let plugins define richer schemas or aggregates.

### Alternative 3: Normalize only operation events and leave other events as-is

- **Pros**: Smaller change than rewriting all observability messages.
- **Cons**: Leaves the exact inconsistency this ADR addresses: some structures have context, some only have operation id, and some have neither.
- **Why not**: Plugin authors and the sidecar projection path need one observability common-field rule, not a special case per event family.

### Alternative 4: Put grouping, aggregation, and export policy in the server

- **Pros**: Could offer one default traces/logs/metrics behavior and precomputed summaries to every deployment.
- **Cons**: Makes core responsible for plugin-specific aggregation/export choices and turns observability into a server subsystem rather than a plugin capability.
- **Why not**: Explicitly rejected. GoCache's microkernel model streams operation-correlated typed records; plugins calculate, aggregate, optimize, and export however they want.

### Alternative 5: Expose server-only connection context versions to plugins

- **Pros**: Preserves exact server-side context version references across the GCPC boundary.
- **Cons**: IPC plugins do not own client connection context versions and cannot safely interpret or manage those lifetimes.
- **Why not**: Connection context versioning is a server-side performance mechanism. Plugins receive serialized filtered telemetry context or produce their own plugin context.

## Consequences

### Positive

- Gives every observability record a consistent operation/context/source surface without imposing a server-owned telemetry product schema.
- Keeps known event details in generated typed oneof payloads while allowing worker-side lazy projection from OperationTracker records.
- Keeps the server as a generic streamer/filter/projection boundary, not an aggregator or grouped-telemetry calculator.
- Lets plugins implement OTLP, Prometheus-style aggregation, audit, or custom event processing without server-specific aggregate variants.
- Makes context materialization and plugin feedback-loop prevention explicit in the wire contract.

### Negative

- Broad compile-time blast radius: `api/gcpc/v1`, `api/events`, `pkg/events`, `pkg/pipeline`, `pkg/logcollector`, `pkg/plugin/manager`, `sdk/pluginsdk`, `plugins/instrumentation`, `plugins/pubsub`, and generated protobuf users must change together.
- Existing tests and docs that assert old event names, missing common fields, or legacy payload shapes must be rewritten.
- Existing plugins that switch on typed `EventV1` payloads must adapt to the new common fields and serialized context; this breakage is accepted because backward compatibility is not a goal for the current development branch.
- Field-level details such as operation id strategy output and common context representation must be finalized before implementation.

### Risks

- **Risk**: The common surface drifts back into an unbounded generic map. **Mitigation**: Keep a small fixed common field set, preserve typed oneof payloads, bound extension/context size, and test required fields for server-produced records.
- **Risk**: Event payloads grow large and recreate allocation pressure. **Mitigation**: Keep payloads bounded, use lazy projection, and benchmark event construction/projection after OperationTracker integration.
- **Risk**: Event-type-only subscription sends too much data. **Mitigation**: Start with type routing because it is the accepted first scope; add source/provenance or delivery filters only after measurements justify the complexity.
- **Risk**: Plugin-emitted telemetry loops back into exporter plugins. **Mitigation**: Include source/provenance fields and default self-exclusion for plugin-produced telemetry.
- **Risk**: Rewriting `gcpcv1` in place makes old docs/tests misleading during migration. **Mitigation**: Update ADRs, GCPC docs, plugin docs, diagrams, and generated code in the same logical unit as the schema change.
- **Risk**: Context carried through GCPC is confused with server connection context versions. **Mitigation**: Document that GCPC carries serialized filtered telemetry context, while connection context versions are server-only and never serialized.
