---
title: Prometheus Plugin
description: Prometheus metrics exporter — per-command counters, latency histograms, /metrics endpoint
status: living
last_updated: 2026-05-30
related:
  - Plugins
  - ADR-0023
  - ADR-0028
---

# Prometheus Plugin

Prometheus metrics exporter for GoCache. The current prototype subscribes to asynchronous command-completion events, records per-command counters and latency histograms, and serves a `/metrics` HTTP endpoint in Prometheus text exposition format. ADR-0028 targets cheap `operation.completed` summaries instead of hook-phase `command.post` taxonomy for the next event contract.

It does **not** register command hooks or operation hooks. Hooks remain reserved for denial/enrichment plugins; runtime metrics stay off the blocking command path.

## Quick Start

1. Build the plugin:
   ```bash
   task build:plugins
   ```

2. Enable in `gocache.yaml`:
   ```yaml
   plugins:
     enabled: true
     dir: "bin/plugins"
     overrides:
       prometheus:
         critical: false
         priority: 100
         scopes:
           - "events"
           - "server:query:health"
           - "server:query:plugins"
   ```

3. Start the server:
   ```bash
   ./bin/gocache-server
   ```

4. Query metrics:
   ```bash
   curl http://localhost:9100/metrics
   ```

## Configuration

| Setting | Source | Default | Description |
|---------|--------|---------|-------------|
| HTTP port | `PROMETHEUS_PORT` env var | `:9100` | Address for the metrics HTTP server |
| Critical | `gocache.yaml` override | `false` | Plugin crash does not affect the server |
| Scopes | `gocache.yaml` override | `events`, `server:query:health`, `server:query:plugins` | Event subscription plus health/readiness queries |

## Metrics

### `gocache_commands_total` (counter)

Total number of commands processed, labeled by command name.

```text
gocache_commands_total{command="SET"} 1234
gocache_commands_total{command="GET"} 5678
```

### `gocache_command_errors_total` (counter)

Total number of commands that returned an error, labeled by command name.

```text
gocache_command_errors_total{command="SET"} 2
```

### `gocache_command_duration_seconds` (histogram)

Command execution latency in seconds, labeled by command name. The current prototype uses server-measured timing from `CommandPostEventV1.elapsed_ns`; the ADR-0028 target contract derives this from `operation.completed` summary fields.

Bucket boundaries: 1ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s.

```text
gocache_command_duration_seconds_bucket{command="SET",le="0.001"} 100
gocache_command_duration_seconds_bucket{command="SET",le="0.005"} 200
...
gocache_command_duration_seconds_bucket{command="SET",le="+Inf"} 1234
gocache_command_duration_seconds_sum{command="SET"} 12.34
gocache_command_duration_seconds_count{command="SET"} 1234
```

### `gocache_plugin_info` (gauge)

Plugin metadata.

```text
gocache_plugin_info{name="prometheus",version="0.1.0"} 1
```

## How It Works

The plugin implements `EventPlugin` and currently subscribes only to command-completion telemetry. Under ADR-0028, it should request the cheapest `operation.completed` detail level that contains metrics-safe command name, elapsed time, and error status. For every command completion record it captures:

- command name
- server-measured elapsed nanoseconds
- whether the command returned an error

The HTTP handler renders the accumulated metrics in Prometheus text format on demand. No external Prometheus client dependency is used; the text format is written directly.

OTLP traces/logs/events are intentionally out of scope for this plugin. A separate `instrumentation` plugin should own runtime OTLP export.

Prometheus should not require full command args, key values, hook context maps, operation IDs as metric labels, or log records. Its subscription should contribute only the interest mask needed for low-cardinality completion summaries.

## Design Diagrams

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [Architecture](design/component/components_prometheus.puml) | Plugin internal structure |
| Sequence | [Metrics Collection](design/sequence/sequence_metrics_collection.puml) | Data flow from `command.post` event to `/metrics` |
