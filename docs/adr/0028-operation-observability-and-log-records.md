---
title: ADR-0028 Operation Observability and Log Records
description: Make operation lifecycle events the canonical observability contract and move high-volume logs to a separate bounded diagnostic signal
status: proposed
date: 2026-05-30
deciders: [witherxse]
related:
  - 0022-modular-performance-budget
  - 0023-lifecycle-otlp-and-runtime-instrumentation
  - 0024-async-event-delivery-and-command-reaction-points
  - 0026-event-traffic-classes-and-backpressure
  - 0027-event-replay-cursors-and-gaps
  - Performance
  - Plugins
---

# ADR-0028: Operation Observability and Log Records

## Context

GoCache's observability path needs a public contract before the async event hot path is optimized. The current event vocabulary mixes architectural signals with hook-phase names such as `command.pre` and `command.post`, and current log export can convert structured JSON log lines into `log.entry` events. That shape is useful for a prototype, but it makes logs compete with command/runtime telemetry on the same event bus even though logs are likely to be the highest-volume observability records.

The thesis claim is observability, modularity, and extensibility with low performance cost. ADR-0022 sets a modular performance budget, ADR-0024 keeps events asynchronous and observational, ADR-0026 requires explicit backpressure/drop accounting, and ADR-0027 prevents high-rate telemetry replay from pretending to be complete. This ADR defines the taxonomy and log handling model that those delivery mechanisms carry.

Existing async event IPC measurements show that the first optimization target is producer-side event/log construction, not only transport backpressure. In the current evidence set, disabling event production nearly recovers the no-IPC control path, while disabling only the plugin bridge leaves a much larger deficit. That means the public contract must let the server avoid building command args, context maps, log entries, protobuf payloads, and subscriber-specific projections unless an interested consumer actually requested that detail.

The repository currently uses `operation.start` and `operation.complete` in some implementation and documentation paths. This ADR deliberately corrects the public contract to the clearer lifecycle names `operation.started` and `operation.completed`; no migration ceremony is required while the thesis implementation is still in development.

## Decision

GoCache uses server-owned operations as the canonical public observability model. Every recorded action starts with `operation.started` and ends with `operation.completed`; other records are child facts or diagnostics correlated to that operation, not replacement lifecycle names.

High-volume logs are not default event-bus events. Logs use a separate correlated `LogRecord`/diagnostic signal that carries `operation_id`, parent operation context when available, level, component, message template or message, and bounded attributes. Log delivery is best-effort and non-blocking by default: level/component filters, producer-side interest masks, lazy field construction, batching, sampling, bounded queues, drop counters, and shutdown flush are part of the contract.

The event bus remains for operation lifecycle and opt-in child facts that plugins explicitly subscribe to. Subscriptions contribute to an aggregate interest mask that producers can check before doing optional work. Access-log-style records should be derived from `operation.completed` by an exporter when requested. Diagnostic logs should be exported through the log-record pipeline, not by first stringifying/parsing JSON and then re-emitting every log line as an event.

The public event, log, and GCPC payloads are materialization targets, not the default in-memory carrier on the command path. The command path should update compact internal summaries first, then project those summaries into public events, log records, metrics updates, or protobuf messages only after the relevant interest mask has been checked.

## Contract Shape

### Operation lifecycle

- `operation.started` is the entry signal for any recorded server action.
- `operation.completed` is the exit signal for that same action and carries status, duration, failure reason, and cheap summary facts.
- Operation IDs are public opaque correlation identifiers. Internal implementation may later optimize ID allocation or storage without changing the external contract.
- Parent/child relationships are expressed through operation IDs and parent IDs, not through event-type naming.

### Child facts

Child facts are exact opt-in structured observations attached to an operation. Examples include command result summaries, connection/auth decisions, plugin lifecycle facts, replay gaps, and metrics aggregation inputs.

Child facts must not use hook-phase names such as `command.post` as their taxonomy. Hook phase names describe synchronous reaction points; observability names describe recorded server facts.

### Interest masks and producer-side gating

Interest masks are the main hot-path optimization for operation child facts and log detail. The server maintains an aggregate, atomically readable view of what plugins/exporters currently want. Producers check that view before constructing optional payloads.

The default execution path should therefore be:

1. create the minimal operation lifecycle state needed for correctness and correlation;
2. check whether anyone is interested in the exact signal class and detail level;
3. skip optional payload work if there is no interest;
4. construct only the requested fields when interest exists;
5. enqueue to the signal's bounded delivery path with visible drop accounting.

Interest dimensions should be explicit and low-cost to test. Candidate dimensions include signal class (`operation`, child fact, log record), operation type, component, log level, detail level, and delivery class. The mask must not require scanning subscriber lists, allocating maps, serializing command args, copying hook context, or computing high-cardinality labels on the command path.

Interest masks are also how GoCache keeps observability modular: a Prometheus plugin can ask for cheap completion summaries; an OTLP/log exporter can ask for sampled diagnostics; a short debugging window can ask for richer fields. Those choices must not tax the no-subscriber or cheap-subscriber path.

### Producer summaries and lazy projection

The expected implementation model is a compact producer record followed by lazy projection:

- static command facts live in precomputed descriptors rather than per-command maps;
- per-command runtime facts live in a stack-local or otherwise allocation-minimal completion summary;
- common correlation and REX fields use fixed slots where possible;
- common optional fields use typed field arrays and bitsets/flags instead of generic maps;
- optional enrichment uses a lazy extension map or overlay that is nil on the common path;
- operation objects, context snapshots, event structs, log records, and protobuf messages are materialized only for hooks, queries, or subscribers that require them;
- subscriber/exporter projections are built directly from summaries instead of cloning a full event and then filtering it.

This keeps the operation-first contract while avoiding the false requirement that every command eagerly allocate a public operation/event/log object. Prometheus-style metrics should consume cheap completion projections. Rich traces, audit records, and diagnostic logs may request additional detail, but their cost must remain explicit and measurable.

The implementation must not replace one hot-path map with another mutable shared map. `sync.Map` is not the default context representation, and `sync.Pool` must be limited to transient buffers or encoders with explicit reset rules. Request-owned operation/context state must not be returned to a pool while any hook, log, event, protobuf projection, or subscriber can still reference it.

### GCPC and protobuf materialization

GCPC is the plugin wire contract, not the internal hot-path representation. Event/log producers should avoid constructing generated protobuf oneof payloads, repeated arg slices, context maps, and envelopes until a subscriber-specific projection is needed. The bridge should prefer direct projection into the final outbound payload over `proto.Clone` followed by filtering.

Batching and serialization optimizations belong after producer costs are reduced. `EventBatchV1`-style batching, `proto.MarshalOptions.MarshalAppend`, header reuse, and future stream-topology/yamux work can reduce downstream delivery overhead, but they do not replace the producer-side interest-mask and lazy-projection requirement.

### Other hot-path optimizations

The contract assumes these implementation optimizations before any event/log path is considered performance-acceptable:

- no subscriber means no optional event/log payload construction;
- no matching detail interest means no command args, key snapshots, context map copies, or message formatting;
- completion summaries prefer fixed fields and small enums over generic maps;
- batching amortizes IPC writes for high-rate subscribers;
- sampling/coalescing is available for noisy diagnostics;
- drop/gap counters are updated cheaply and exposed outside the saturated stream;
- redaction happens before export and should be avoided entirely for fields that were never requested;
- metrics labels stay low-cardinality, with operation IDs kept as payload correlation fields only.

### Log records

`LogRecord` is a separate diagnostic signal, correlated with operations but delivered through log-specific controls:

- level and component filtering before record construction when possible;
- lazy attribute construction so disabled logs do not allocate command args, key snapshots, context maps, or stringified payloads;
- message formatting and expensive attribute functions only after the level/component/detail interest gate passes;
- bounded per-exporter queues with explicit overflow policy;
- batching and flush intervals for IPC/exporter efficiency;
- sampling or coalescing for noisy repeated diagnostics;
- drop counters and last-drop metadata exposed outside the saturated log stream;
- best-effort shutdown flush with tight deadlines.

The end state removes the logger-to-JSON-pipe-to-logcollector-to-`log.entry` path from the hot observability model. Human stderr/stdout output may remain as a local mirror or fallback, and raw plugin stdout may remain a best-effort diagnostic fallback, but neither should be the authoritative structured log transport for high-rate observability.

By default, log records do not include command arguments, key values, hook context maps, or high-cardinality labels. Exporters may request additional detail explicitly, subject to redaction and the performance budget.

### Delivery classes

The taxonomy maps onto the ADR-0026 delivery model:

| Signal | Default delivery | Replay/history | Primary consumers |
|---|---|---|---|
| `operation.started` | async event, opt-in, bounded | live plus explicit cursor/gap policy | tracing, operation tree reconstruction |
| `operation.completed` | async event, opt-in, bounded | live plus explicit cursor/gap policy | metrics, access-log exporters, tracing completion |
| child fact events | async event, exact opt-in, bounded | per-class policy | metrics, diagnostics, feature-specific plugins |
| `LogRecord` diagnostics | separate bounded log pipeline | live or exporter-managed retention | logging backends, OTLP logs, local diagnostics |
| drop/gap counters | server query / metrics surface plus optional event summary | latest aggregate | operators, health checks, exporters |

### Measurement obligations

The implementation that accepts this ADR must prove the cost model with attribution before broad transport work:

- package benchmarks with `-benchmem` for operation creation/enrichment/snapshotting, event constructors, pipeline no-sink versus full-sink paths, event bus emit, bridge projection, transport marshal/framing, log parsing/replacement, and Prometheus aggregation;
- docker benchmark comparisons that preserve the existing `full`, `events-off`, and `bridge-off` modes so producer and downstream costs stay separable;
- CPU and heap profiles only after benchmark modes identify the stage being measured;
- `plugin.ipc`/delivery counters for queue-full drops, enqueue latency, write latency, lag, and accepted/drop totals;
- a stop gate that prevents yamux or stream-topology work from being treated as the first fix while producer construction still dominates.

## Alternatives Considered

### Alternative 1: Keep `command.post` as the main command observability event

- **Pros**: Smallest change from the current Prometheus prototype and easy to connect to post-hook mental models.
- **Cons**: Blurs hook semantics with event semantics and makes the event taxonomy describe where data was observed instead of what server action was recorded.
- **Why not**: The public contract should be operation-first. Command post-hooks remain reaction points; completed command operations should surface through `operation.completed` and explicit child facts.

### Alternative 2: Keep every log line as a `log.entry` event

- **Pros**: One subscription mechanism for all observability data and simple plugin implementation.
- **Cons**: Logs are usually the highest-volume records, so this turns the event bus into the log pipeline and can recreate the IPC hot-path overhead that the async event work is trying to remove.
- **Why not**: Logs need log-specific controls: level checks, lazy fields, batching, sampling, and drop counters. Those controls are easier and cheaper when logs are a separate correlated signal.

### Alternative 3: Drop runtime logs from the export contract

- **Pros**: Lowest overhead and simplest event contract.
- **Cons**: Undermines the observability part of the thesis and leaves operators without diagnostic context when metrics or traces are insufficient.
- **Why not**: Logs still need to be exported somehow. The right answer is an optimized log-record path, not removal.

### Alternative 4: Put full command args, keys, and context in every operation completion

- **Pros**: Very rich access logs and easy downstream debugging.
- **Cons**: High allocation cost, privacy risk, high-cardinality payloads, and excessive IPC volume on the hottest path.
- **Why not**: Rich payloads must be requested explicitly. The default path should carry cheap summaries and correlation IDs.

### Alternative 5: Treat logs as child operations

- **Pros**: Preserves a pure tree model where every observation is an operation node.
- **Cons**: Turns each log line into lifecycle state, doubling records and making operation trees noisy and expensive.
- **Why not**: Logs are diagnostic attributes over time, not server actions with their own start/complete lifecycle in the common case.

## Consequences

### Positive

- Makes the public observability model match the thesis: server-owned operations are the stable contract; plugins choose how much detail to subscribe to.
- Targets the largest event-volume risk directly by optimizing logs outside the generic event bus.
- Lets Prometheus-style metrics derive cheap counters from `operation.completed` without depending on hook names.
- Keeps tracing/logging/metrics correlated through operation IDs while preserving separate delivery controls per signal.
- Aligns log overload behavior with ADR-0026 and ADR-0027: loss is best-effort, visible, and bounded rather than hidden behind command latency.

### Negative

- Requires API, GCPC, SDK, and plugin docs to distinguish event subscriptions from log-record subscriptions.
- Existing prototype code that emits or consumes `log.entry`, `command.pre`, `command.post`, `operation.start`, or `operation.complete` as public observability events must be renamed or reshaped before implementation is accepted.
- Exporters that want rich diagnostic context must negotiate/request detail instead of assuming every log or operation carries it.

### Risks

- **Risk**: Splitting logs from events makes plugin authoring feel more complex. **Mitigation**: SDK helpers should expose the same correlation fields and simple subscription handlers while keeping the underlying delivery classes separate.
- **Risk**: Best-effort log delivery hides important failures during overload. **Mitigation**: Expose drop counters, last-drop metadata, and exporter health through server queries/metrics that do not depend on the saturated log stream.
- **Risk**: The operation-completed path grows into an access-log payload and reintroduces allocations. **Mitigation**: Keep default fields cheap and gate rich fields behind interest/detail masks with benchmarks.
- **Risk**: Operation IDs become high-cardinality labels in metrics backends. **Mitigation**: Treat operation IDs as correlation payload fields, not metric labels or routing keys.
- **Risk**: Sampling drops the exact diagnostic line needed for debugging. **Mitigation**: Allow scoped level/component/detail overrides for short diagnostic windows and record sampling/drop policy in exported metadata.
