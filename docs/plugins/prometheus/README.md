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

Prometheus metrics exporter for GoCache. It serves `/metrics`, `/healthz`, and `/readyz` over HTTP; core command-dispatch counters and latency histograms are pulled from the generic `server:query:metrics.commands` aggregate snapshot instead of receiving one IPC event per command.

It does **not** register command hooks, operation hooks, or event subscriptions for runtime metrics. Hooks remain reserved for denial/enrichment plugins; exact event streams remain available for plugins that need full records.

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
           - "server:query:metrics.commands"
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
| Scopes | `gocache.yaml` override | `server:query:metrics.commands`, `server:query:health`, `server:query:plugins` | Pull-based command metrics plus health/readiness queries |

## Metrics

### `gocache_commands_total` (counter)

Total number of core commands processed, labeled by command name.

```text
gocache_commands_total{command="SET"} 1234
gocache_commands_total{command="GET"} 5678
```

### `gocache_command_errors_total` (counter)

Total number of core commands that returned an error, labeled by command name.

```text
gocache_command_errors_total{command="SET"} 2
```

### `gocache_command_duration_seconds` (histogram)

Command execution latency in seconds, labeled by command name. The server records elapsed nanoseconds into a compact command-metrics collector only while a `server:query:metrics.commands` consumer is registered; the plugin converts that aggregate snapshot to Prometheus seconds during scrape.

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

The plugin implements `QueryPlugin` and receives an SDK session after registration. For each `/metrics` request, it queries the server topic `metrics.commands`; the server checks the `server:query:metrics.commands` scope and returns aggregate rows containing:

- command name
- total command count
- error count
- sum of elapsed nanoseconds
- histogram bucket counts

The HTTP handler replaces the local collector snapshot from that response and renders Prometheus text format on demand. No external Prometheus client dependency is used; the text format is written directly.

OTLP traces/logs/events are intentionally out of scope for this plugin. A separate `instrumentation` plugin should own runtime OTLP export.

Prometheus does not require full command args, key values, hook context maps, operation IDs as metric labels, or log records. Its query scope enables low-cardinality aggregate metrics without forcing command event protobufs or per-plugin event projection on every command.

## Design Diagrams

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [Architecture](design/component/components_prometheus.puml) | Plugin internal structure |
| Sequence | [Metrics Collection](design/sequence/sequence_metrics_collection.puml) | Pull flow from `server:query:metrics.commands` to `/metrics` |
