---
title: ADR-0023 Lifecycle OTLP and Runtime Instrumentation Split
description: Scope embedded lifecycle OTLP to pre-IPC failure visibility and move runtime observability to event-only IPC instrumentation
status: accepted
date: 2026-05-28
deciders: [witherxse]
related:
  - 0006-builtin-vs-third-party-transport
  - 0022-modular-performance-budget
  - Plugins
  - Performance
---

# ADR-0023: Lifecycle OTLP and Runtime Instrumentation Split

## Context

GoCache needs cloud-native observability without losing the microkernel performance budget. The earlier embedded `otlp` plugin was created so startup failures and panic/shutdown paths could reach Grafana/Tempo/Jaeger before any IPC plugin had connected. The former IPC observability plugin had grown to include Prometheus metrics, operation hooks, log-event enrichment, and OTLP trace export, which made the runtime observability boundary unclear and added hot-path IPC overhead.

Compiled Go binaries cannot rely on Java/.NET-style runtime agents for application-semantic instrumentation. Core must explicitly create and propagate trace context, and runtime telemetry must be emitted through deliberate low-overhead call sites. ADR-0022 sets a <=20% modular overhead budget, so runtime observability cannot depend on synchronous per-operation hooks unless it is explicitly critical.

## Decision

The embedded OTLP plugin is renamed and scoped as `lifecycleotlp`. It exists only for process-lifecycle telemetry that an IPC plugin cannot reliably observe: process start, config load, plugin-runtime startup failures, fatal boot failures, panic/crash-adjacent markers, shutdown spans, and best-effort force flush.

Runtime observability moves to IPC plugins that subscribe to asynchronous server events rather than command hooks or operation hooks. Core owns trace context extraction/generation from REX metadata or incoming protocol metadata and attaches that context to operation/event metadata, but core does not own OTLP runtime exporters. Prometheus metrics and OTLP instrumentation are separate responsibilities: the `prometheus` plugin exposes pull-based metrics, while a future `instrumentation` plugin should batch/sample/export traces, logs, and runtime events.

## Alternatives Considered

### Alternative 1: Keep generic embedded `otlp` as the observability owner

- **Pros**: Earliest possible startup visibility and no IPC overhead for runtime spans.
- **Cons**: Couples runtime observability/export dependencies into the server binary, removes crash isolation for exporter bugs, and blurs embedded-vs-IPC responsibilities.
- **Why not**: Embedded plugins are reserved for pre-config/pre-IPC capabilities. Runtime telemetry is reachable after plugin startup and should keep IPC isolation.

### Alternative 2: Keep all observability in `prometheus`

- **Pros**: One plugin already contains Prometheus metrics, OTLP tracing, readiness, and log-event enrichment.
- **Cons**: The name is too broad, the code mixes metrics and tracing, and current operation-hook use creates unacceptable hot-path overhead.
- **Why not**: The runtime path must become event-only and batched. Prometheus pull metrics and OTLP traces/logs have different semantics, pressure profiles, and deployment expectations.

### Alternative 3: Remove embedded lifecycle OTLP entirely

- **Pros**: Simpler binary and one fewer observability path.
- **Cons**: Startup config failures, plugin-manager failures, and panic/shutdown windows can disappear before an IPC plugin is available.
- **Why not**: The original t=0 visibility requirement is still valid. The fix is narrow scope and naming, not deletion.

### Alternative 4: Replace GCPC with a general RPC or messaging system for telemetry

- **Pros**: Mature flow-control, observability, and ecosystem tooling.
- **Cons**: Adds framework overhead to the local hot path and changes the plugin deployment model before proving GCPC cannot meet the budget.
- **Why not**: Current evidence points to synchronous hook/event shape and local scheduling/allocation costs, not missing distributed messaging features.

## Consequences

### Positive

- Clarifies the embedded plugin boundary: `lifecycleotlp` is a pre-IPC failure-visibility component, not the runtime observability system.
- Preserves early Grafana/Tempo/Jaeger visibility for failures that IPC plugins cannot see.
- Splits the former combined observability surface into honest plugin responsibilities: `prometheus` for metrics and `instrumentation` for future OTLP runtime export.
- Aligns runtime observability with ADR-0022 by requiring async event delivery, batching, sampling, and drop accounting instead of blocking hooks.
- Keeps core cloud-native by owning trace continuity while avoiding exporter coupling.

### Negative

- Requires renaming build tags, package paths, environment variables, and documentation from generic `otlp` to `lifecycleotlp`.
- Requires a separate `instrumentation` plugin before runtime OTLP export returns.
- Requires new event schemas and benchmark gates before hook-based tracing can be removed.

### Risks

- **Risk**: Lifecycle and runtime exporters emit duplicate boot/config/shutdown spans. **Mitigation**: Mark lifecycle records with `gocache.component=lifecycle_otlp`, give every process a run identifier in the future event schema, and make the runtime instrumentation plugin ignore lifecycle-only sources unless explicitly configured.
- **Risk**: Event-only instrumentation drops telemetry under overload. **Mitigation**: Use bounded queues, sampling, and explicit dropped-event counters; reserve blocking behavior for critical hooks only.
- **Risk**: Crash-path flushing is assumed to be reliable. **Mitigation**: Treat `ForceFlush` during panic/shutdown as best effort, keep tight timeouts, and pair lifecycle OTLP with crashdump/local markers for hard failures.
- **Risk**: One plugin connection becomes a head-of-line bottleneck for event traffic. **Mitigation**: First benchmark async event delivery; if still over budget, evaluate GCPC logical streams or per-traffic-class connections as described in the modular overhead plan.
