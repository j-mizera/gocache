---
title: ADR-0033 Common OperationTracker Sharding Contract
description: Share pointer-free OperationTracker engine primitives from commons while keeping server projection and context-version state internal
status: accepted
date: 2026-06-01
deciders: [witherxse]
related:
  - 0021-commons-package-layer
  - 0022-modular-performance-budget
  - 0026-event-traffic-classes-and-backpressure
  - 0029-operationtracker-sidecar-low-overhead-telemetry
  - 0031-operation-identity-export-contract
  - 0032-telemetry-context-version-ownership
  - projects/gocache/research/gocache-zero-allocation-telemetry-sidecar-implementation-brief.md
---

# ADR-0033: Common OperationTracker Sharding Contract

## Context

ADR-0029 accepts the OperationTracker sidecar direction, but the updated implementation brief and user decision add two concrete constraints. First, the sidecar engine should be reusable from common code rather than trapped inside one server package. Second, the engine is not one global ring that processes every record; it supports multiple OperationTracer instances and routes records by optimized internal operation id so work is spread across sidecar workers. At the same time, GoCache must preserve plugin isolation: plugins may import `api/`, `sdk/`, and `commons/`, but must not import server internals under `pkg/`.

This ADR covers package placement, sharding, and the common engine boundary. It does not address connection id formatting or a future event-system decoupling; those are not current concerns.

## Decision

GoCache places reusable pointer-free OperationTracker engine primitives in `commons/observability`, while keeping server-owned projection, event/GCPC fanout, subscriber queues, concrete `ClientContext` mapping, redaction, and lifecycle integration in `pkg/`. The shared common layer may include record layouts, ring mechanics, shard routing helpers, operation identity strategy implementations, minimal producer/consumer interfaces, and generic in-memory context-version primitives needed by the manager-owned submit/reap contract. It must not include concrete server connection state, plugin manager logic, event-bus coupling, GCPC/log materialization, zerolog calls, or backend/export aggregation.

`api/observability` remains interface/value-contract only. It defines what plugins and SDK helpers can depend on, not how the server implements the sidecar. `commons/observability` is the reusable implementation layer for code that can safely be shared by server, SDK, embedded plugins, or IPC plugin tooling without importing server internals.

The sidecar runs a configurable number of OperationTracer instances. Records are assigned to tracer instances by the optimized internal operation handle from ADR-0031, using a deterministic sharding rule such as modulo or a stable hash. This keeps a single operation's records on one tracer while distributing concurrent operations across workers. The configured tracer count, ring capacity, and worker scheduling are server configuration and benchmark parameters, not plugin-controlled API.

The common engine remains generic. It submits and drains records in FIFO order; it does not calculate grouped telemetry, aggregate metrics, create traces, export backend-specific data, or own telemetry schemas. Reap callbacks later convert operation kinds to runtime/GCPC operation output, fold context mutation records into worker-owned context state before subsequent log/event records, and materialize runtime logs/zerolog output off the command goroutine. Plugins may use common primitives for plugin-local background operation telemetry, but server-side `ClientContext` extraction, redaction, GCPC projection, and plugin fanout remain server-owned. Plugin-local internal handles are local to the plugin-side engine instance; they are not server internal handles and never cross GCPC.

## Contract Shape

### Package boundary

- `api/observability`: stable value types and interfaces visible to plugins.
- `commons/observability`: reusable pointer-free records, ring/tracer primitives, shard routing helpers, UUIDv7/W3C identity strategy implementations, generic manager-owned context-version primitives, FIFO drain surfaces, and allocation-sensitive tests/benchmarks.
- `pkg/...`: server OperationTracker sidecar wiring, concrete `ClientContext` to telemetry-context mapping, redaction/filtering, worker projection, event/GCPC adapters, subscriber queues, overload reporting, zerolog/log emission, and lifecycle/reaper scheduling logic.
- `sdk/...`: plugin ergonomics for starting plugin-owned operations and producing typed records without importing `pkg/`.

### Sharding

Every operation has an internal numeric handle. The OperationTracker engine routes records for that handle to exactly one OperationTracer instance. The implementation may use per-producer rings, per-tracer queues, or a hybrid layout, but the contract is:

1. no shared mutex on the command hot path;
2. no protobuf/map/string formatting on the command hot path;
3. no cross-tracer split for one operation's ordered record stream;
4. configurable tracer count for benchmark tuning and deployment sizing.

### Overload visibility

Telemetry is fire-and-forget and may drop under overload. The common engine should therefore prefer bounded non-blocking submission over forcing command goroutines to wait for telemetry. Dropped records are acceptable for telemetry, but they must not masquerade as complete telemetry. The implementation should expose cheap counters, last-drop metadata, health-query data, or gap records where practical so operators and plugins can see that the record stream is incomplete.

This ADR does not turn the server into a telemetry backend or durable audit log. It only defines the common sidecar engine boundary, sharding contract, and minimum visibility expectation for overload.

## Accepted Scope and Follow-up Gates

Accepted on 2026-06-02 for Subplan A's `api/observability` and `commons/observability` package split, sharded manager contract, FIFO drain surfaces, identity strategies, generic context-version primitives, and allocation-sensitive package benchmarks. Server projection, GCPC/log materialization, plugin fanout, and lifecycle scheduling remain Subplan B.

The following gates remain before full server integration:

1. **Server wiring**: keep `pkg/` responsible for server `ClientContext` mapping, projection, redaction, GCPC/log emission, plugin fanout, and lifecycle scheduling.
2. **Shard routing validation**: continue aligning shard routing with ADR-0031's internal numeric handle and exported identity mapping as server wiring adopts the common manager.
3. **Overload visibility**: define cheap counters, gap metadata, or health-query data that make drop-allowed telemetry visibly incomplete without blocking command execution.
4. **Plugin-local use**: document that plugin-local handles are never server handles and cannot be used for server operation lookup.
5. **Server benchmarks**: extend allocation and throughput benchmarks from common ring/tracer primitives to server hot paths before claiming runtime improvements.

## Alternatives Considered

### Alternative 1: Keep all tracker code in `pkg/`

- **Pros**: Maximum freedom to change server internals.
- **Cons**: Blocks plugin-local use of the same zero-allocation primitives and duplicates engine mechanics if plugins need background operation telemetry.
- **Why not**: The user explicitly wants the sidecar OperationTracker engine available from common code.

### Alternative 2: Put the concrete server tracker in `api/` or expose it directly to plugins

- **Pros**: One public type for everyone.
- **Cons**: Freezes server internals, leaks connection context versioning, and violates the microkernel boundary.
- **Why not**: Plugins need generic producer capabilities, not the server's mutable tracker and projection state.

### Alternative 3: Use one global ring and one global worker

- **Pros**: Simplest implementation and ordering model.
- **Cons**: Recreates a single serialization point and cannot use the internal operation id sharding already supported by the reference design.
- **Why not**: The sidecar exists to remove the current shared event-bus convoy; a global ring risks rebuilding it.

### Alternative 4: Rewrite the event system first

- **Pros**: Could cleanly separate reaction events from telemetry events before sidecar integration.
- **Cons**: Large blast radius and not required to prove the thesis-critical performance path.
- **Why not**: Event-system tight coupling is acknowledged but intentionally out of scope for now.

### Alternative 5: Make the common engine enforce durable/no-loss telemetry

- **Pros**: Strong completeness guarantees and easier replay reasoning.
- **Cons**: Pushes WAL/backpressure/audit semantics into a low-overhead telemetry engine and risks command-path latency.
- **Why not**: Telemetry is not audit-durable in this ADR set. Durable event handling belongs to a future event/audit system.

## Consequences

### Positive

- Makes the zero-allocation tracker engine reusable without exposing server internals.
- Allows OperationTracer count and ring sizing to be benchmarked as explicit performance knobs.
- Preserves per-operation ordering for accepted records while distributing operations across workers.
- Keeps plugins on supported import boundaries: `api/`, `sdk/`, `commons/`, never `pkg/`.
- Keeps API stable by putting contracts in `api/observability` and concrete reusable implementation in `commons/observability`.

### Negative

- Creates a new common package that must avoid accidental server-specific dependencies.
- Requires careful API design so common primitives stay allocation-free and pointer-free where promised.
- Requires tests/benchmarks in both common engine code and server integration code.
- Requires SDK and GCPC wiring so IPC plugins can use common helpers without seeing server internals.

### Risks

- **Risk**: `commons/observability` grows into a second server-internal package. **Mitigation**: Limit it to generic records/rings/sharding helpers/identity strategies and enforce imports with plugin isolation checks.
- **Risk**: Sharding by operation id causes skew under unusual id allocation patterns. **Mitigation**: Benchmark tracer distributions and allow configurable tracer count/routing strategy.
- **Risk**: Drop-allowed telemetry becomes invisible loss. **Mitigation**: Expose cheap counters/gap metadata/health query state and document that telemetry streams are best-effort, not audit-complete.
