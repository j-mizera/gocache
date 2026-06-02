---
title: ADR-0031 Operation Identity Export Contract
description: Separate low-overhead internal operation handles from server-selected UUIDv7 or W3C exported operation identity strategies
status: proposed
date: 2026-06-01
deciders: [witherxse]
related:
  - 0028-operation-observability-and-log-records
  - 0029-operationtracker-sidecar-low-overhead-telemetry
  - 0030-gcpc-v1-observability-context-contract
  - 0032-telemetry-context-version-ownership
  - 0033-common-operationtracker-sharding-contract
  - projects/gocache/research/gocache-zero-allocation-telemetry-sidecar-implementation-brief.md
---

# ADR-0031: Operation Identity Export Contract

## Context

GoCache currently generates operation ids as strings such as `cmd_1`, `conn_1`, `boot_1`, and `pstart_1` in `api/operations`. Those ids are easy to inspect, but they are tied to local counters, allocate/format on the command path, and are not enough for plugin self-assignment once plugins create child or background operations. The sidecar design in ADR-0029 needs optimized internal numeric identifiers for sharding and worker lookup, while GCPC and plugins need a stable exported identity that can follow common tracing formats. The implementation brief also requires one configured exported operation id shape to be respected by every telemetry-capable component.

This ADR does not change connection id formatting. Current local connection ids remain out of scope for this decision.

## Decision

GoCache separates operation identity into two layers:

1. an internal numeric operation handle used for sidecar sharding, worker lookup, parent linkage, ordering, and in-memory state; and
2. an exported operation identity string rendered at API, GCPC, log, event, and plugin boundaries by a server-selected strategy.

The exported identity contract is a strategy pattern. `api/observability` declares the interfaces and value contracts needed by server, SDK, and plugins. `commons/observability` provides the supported strategy implementations. The first supported strategies are UUIDv7 and W3C Trace Context-compatible rendering.

Internal handles are not serialized through GCPC and are not exposed to plugins. Exported operation identities are strings produced by the configured strategy. The strategy input follows the shape of the implementation brief's §15.3 model, but without `NodeID`: it includes the internal handle, root/trace identity material when needed by the selected format, and operation start time/order data needed to render the exported id. The exact Go type names are implementation details; the boundary rule is fixed.

The GCPC server injects the selected identity strategy/configuration into IPC plugins at registration or capability/config delivery time. Embedded plugins receive the server-configured `OperationTracker` and therefore use the already selected strategy. SDK helpers should expose the configured strategy through a telemetry-supporting capability rather than letting each plugin silently pick its own id format.

Parent-child relationships carry both internal and exported references while inside the server. At the boundary, only exported operation ids and exported parent operation ids cross. Plugin-originated background operations use the same configured strategy through SDK/API helpers, so plugin-created children can safely parent to server-created operations without importing server internals. The first implementation does not need to enforce plugin output with a validator in the server; a plugin that ignores the injected format emits bad telemetry and that is a plugin bug.

## Contract Shape

### Internal handle

The internal handle is an integer optimized for the OperationTracker sidecar. It may be `int64`, `uint64`, or a narrow wrapper type, but it must be cheap to allocate, compare, hash, and shard. It is suitable for `handle % tracer_count` routing and map keys in worker-owned state. It is not a public id.

The internal handle remains the authoritative ordering/sharding input. Exported formats such as UUIDv7 or W3C are renderings of operation identity for external correlation; they must not become the worker routing key or the source of ordering truth.

### Exported operation id strategy

The exported id is the public correlation id. GCPC `operation_id`, event records, runtime log records, SDK operation handles, plugin command/hook contexts, and exported telemetry all use the configured exported format.

For UUIDv7, the exported id is a time-aware single identifier useful for logs, indexing, and ordering by creation time. For W3C Trace Context, the exported identity must preserve trace/span semantics rather than treating the trace id alone as a complete operation tree. The W3C strategy may render a `traceparent`-compatible string or the equivalent configured string representation chosen during implementation, but it must remain valid for tracing systems.

### API and commons ownership

`api/observability` owns the strategy interface and any value/input contracts that plugin authors must compile against. `commons/observability` owns the UUIDv7 and W3C strategy implementations, shared tests, and format helpers. Server internals may wrap these strategies for allocation-sensitive operation start, but they must not duplicate public identity contracts in `pkg/`.

### Mapping and lifecycle

The server owns the mapping from internal handles to exported ids. The mapping exists for the lifetime of an operation and any replay/flush window needed by the sidecar. Plugins own exported ids for plugin-originated operations but never receive server internal handles.

### Non-goals

This ADR does not decide connection id formatting, replica/fleet connection identity, event-bus decoupling, trace aggregation, or telemetry backend schema. Those are separate concerns. A later replica-aware identity may add node/fleet data, but `NodeID` is intentionally not part of the first exported operation identity strategy input.

## Priority Review Gates Before Implementation

This ADR remains `proposed` until these identity gates are resolved or explicitly deferred:

1. **Strategy interface shape**: define the `api/observability` interface and value/input contract for rendering UUIDv7 and W3C operation ids without leaking server handles.
2. **Commons implementations**: implement and test UUIDv7 and W3C renderers in `commons/observability`, including format conformance tests.
3. **GCPC configuration propagation**: define how IPC plugins learn the server-selected strategy through GCPC registration/capability/config delivery.
4. **SDK helper behavior**: expose a telemetry-supporting SDK capability so plugins use the injected strategy; document that non-conforming plugin output is a plugin bug in the first implementation.
5. **Legacy test rewrite scope**: plan replacement of `cmd_`, `conn_`, `boot_`, and `pstart_` assumptions in tests/docs without preserving backward compatibility.

## Alternatives Considered

### Alternative 1: Keep prefixed local counters as the exported contract

- **Pros**: Minimal code churn and readable ids in tests.
- **Cons**: Local counters collide across producers, do not support plugin self-assignment without coordination, and encourage string formatting on hot paths.
- **Why not**: The sidecar needs numeric internal handles, and plugins need globally safe exported ids.

### Alternative 2: Expose the internal numeric handle everywhere

- **Pros**: Fastest possible id shape and simplest sharding model.
- **Cons**: Leaks server implementation into plugins and GCPC, blocks common tracing formats, and makes future internal handle changes public breakage.
- **Why not**: Internal sidecar mechanics are not the public operation identity contract.

### Alternative 3: Force W3C Trace Context only

- **Pros**: Best interoperability with distributed tracing tools.
- **Cons**: W3C identity is a trace/span model, not just a single opaque operation string, and it can be heavier than needed for local logs or simple plugin workflows.
- **Why not**: The accepted contract supports W3C, but does not force it as the only strategy.

### Alternative 4: Force UUIDv7 only

- **Pros**: Simple single-id export, time-aware ordering, and good log/index ergonomics.
- **Cons**: Does not encode trace/span relationships or trace flags by itself.
- **Why not**: The accepted contract supports UUIDv7, but W3C-compatible tracing must also be available now.

### Alternative 5: Let each plugin choose its own operation id format

- **Pros**: Maximum plugin autonomy and minimal GCPC configuration work.
- **Cons**: Breaks cross-plugin correlation and makes exporter plugins handle mixed id formats.
- **Why not**: The GCPC server selects and injects the strategy so the telemetry graph uses one operation identity contract.

### Alternative 6: Server-validate or rewrite every plugin id immediately

- **Pros**: Strong enforcement at the boundary.
- **Cons**: Adds implementation cost and can put format parsing on hot or high-volume paths before measurements justify it.
- **Why not**: The first implementation documents non-conforming plugin output as a plugin bug. Enforcement can be added later if real plugins need it.

## Consequences

### Positive

- Keeps OperationTracker hot-path sharding independent from exported id formatting.
- Gives server and plugins one shared export contract instead of ad hoc `cmd_1`/`op_1` fixtures.
- Supports UUIDv7 and W3C-compatible workflows without adopting a full telemetry framework in core.
- Makes plugin-originated child operations safe because plugins can allocate exported ids through the same configured strategy.
- Keeps API contracts in `api/observability` and shared implementations in `commons/observability`.

### Negative

- Requires migration of tests and docs that assert `cmd_`, `conn_`, or `op_` operation id prefixes.
- Requires an internal handle to exported id mapping for operation lifetime and replay windows.
- W3C-compatible exports need careful rendering so trace id and span id semantics are not flattened incorrectly.
- Requires GCPC registration/configuration and SDK helper work before IPC plugins can produce aligned operation ids.

### Risks

- **Risk**: Two identity layers confuse plugin authors. **Mitigation**: Expose only exported ids in API/SDK/GCPC docs; keep internal handles package-private and server-side.
- **Risk**: Different components silently choose different export formats. **Mitigation**: Centralize operation id strategy selection in server configuration and inject the selected strategy into SDK/GCPC plugin flows.
- **Risk**: Exported id generation allocates on the command path. **Mitigation**: Generate or reserve exported ids at operation start through the sidecar/strategy boundary and benchmark allocation counts.
- **Risk**: W3C rendering is implemented as a trace id string without span semantics. **Mitigation**: Add conformance tests that validate W3C traceparent-compatible rendering and parent/span behavior.
