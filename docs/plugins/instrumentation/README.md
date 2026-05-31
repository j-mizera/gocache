---
title: Runtime Instrumentation Plugin
description: IPC plugin that exports GoCache runtime OTLP traces and logs from operation events and runtime log batches
status: living
last_updated: 2026-05-31
related:
  - Plugins
  - GCPC
  - Plugin-Prometheus
  - ADR-0023
  - ADR-0028
---

# Runtime Instrumentation Plugin

`plugins/instrumentation` is the IPC runtime observability exporter for GoCache. It subscribes to operation lifecycle events and batched runtime log events over GCPC, then exports **OTLP traces and OTLP logs** to a collector or Jaeger-compatible OTLP HTTP endpoint.

Runtime metrics are intentionally out of scope. Command counters and latency histograms remain owned by the [Prometheus plugin](../prometheus/README.md) through the pull-based `server:query:metrics.commands` path.

## Scope

The plugin owns:

- runtime spans from `operation.started` and `operation.completed`;
- runtime log export from `runtime.logs` / `RuntimeLogBatchEventV1`;
- replay-gap visibility from `replay.gap`;
- trace correlation from operation IDs and `traceparent` metadata.

The plugin does not own:

- Prometheus metrics;
- lifecycle/pre-IPC startup spans (handled by embedded `lifecycleotlp`);
- command denial, admission, or enrichment hooks;
- a separate Unix-socket/yamux log transport.

## Quick Start

1. Build plugin binaries:

   ```bash
   task build:plugins
   # or directly:
   go build -o bin/plugins/instrumentation ./plugins/instrumentation
   ```

2. Enable plugins and configure the instrumentation override:

   ```yaml
   plugins:
     enabled: true
     dir: "bin/plugins"
     overrides:
       instrumentation:
         failure_policy: continue
         scopes:
           - "events"
         config:
           endpoint: "localhost:4318"
           service: "gocache"
           timeout_ms: 3000
           insecure: true
           disabled: false
   ```

3. Run an OTLP backend such as Jaeger with OTLP HTTP enabled, then start GoCache.

## Configuration

Configuration is delivered through the normal IPC plugin config path (`RegisterAckV1` initial config plus later config reloads) and can be overridden with environment variables.

| Setting | Env var | Default | Description |
|---|---|---|---|
| `endpoint` | `GOCACHE_INSTRUMENTATION_OTLP_ENDPOINT` | empty | OTLP HTTP endpoint host/port, for example `localhost:4318`. Empty disables export. |
| `service` | `GOCACHE_INSTRUMENTATION_OTLP_SERVICE` | `gocache` | OpenTelemetry `service.name` resource value. |
| `timeout_ms` | `GOCACHE_INSTRUMENTATION_OTLP_TIMEOUT_MS` | `3000` | Exporter setup/shutdown timeout in milliseconds. |
| `insecure` | `GOCACHE_INSTRUMENTATION_OTLP_INSECURE` | auto | Use insecure OTLP HTTP. Automatically true for non-HTTPS endpoints. |
| `disabled` | `GOCACHE_INSTRUMENTATION_OTLP_DISABLED` | `false` | Hard off-switch for runtime export. |

When disabled or when `endpoint` is empty, the plugin remains healthy but does not create OTLP providers.

## Event Subscriptions

The plugin implements `EventPlugin` and requests the `events` scope. It subscribes to:

| Event type | Purpose |
|---|---|
| `operation.started` | Start or reconstruct an active span. |
| `operation.completed` | End the span and attach status/duration/failure attributes. |
| `runtime.logs` | Export a periodically flushed batch of runtime log records. |
| `replay.gap` | Export a warning marker when retained event history was incomplete. |

Runtime logs come from `pkg/logcollector`, which parses structured JSON or plain-text log lines into `RuntimeLogRecordV1`, buffers them, and flushes batches periodically with a max-size safety flush. The old per-line `log.entry` event is not part of the runtime contract.

## Trace and Log Correlation

- Operation IDs are the primary runtime correlation key.
- Incoming trace context is extracted from `shared.traceparent`, `shared.rex.traceparent`, or `traceparent` when present.
- Runtime log records with an `operation_id` are attached to the active span context when possible.
- Secret-like fields are redacted before export.

## Verification

Relevant checks:

```bash
go test ./plugins/instrumentation
go test -race ./plugins/instrumentation
go test -tags integration ./plugins/instrumentation
```

The integration-tagged test uses the Jaeger testkit path to verify OTLP export behavior.

## Design Diagrams

| Category | Diagram | Description |
|---|---|---|
| Component | [Runtime Observability](../../gcpc/design/component/components_runtime_observability.puml) | Runtime OTLP plugin, log collector, Prometheus split, lifecycle OTLP split. |
| Sequence | [Runtime OTLP Export](../../gcpc/design/sequence/sequence_runtime_observability_export.puml) | End-to-end operation/log/replay-gap export flow. |
| Component | [Async Event Delivery](../../server/design/component/components_async_event_delivery.puml) | Event queues and runtime telemetry delivery path. |

## Related Docs

- [Plugin overview](../README.md)
- [GCPC protocol](../../gcpc/README.md)
- [Prometheus plugin](../prometheus/README.md)
- [ADR-0023 Lifecycle OTLP and Runtime Instrumentation Split](../../adr/0023-lifecycle-otlp-and-runtime-instrumentation.md)
- [ADR-0028 Operation Observability and Log Records](../../adr/0028-operation-observability-and-log-records.md)
