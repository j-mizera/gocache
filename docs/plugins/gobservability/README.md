# Gobservability Plugin

Prometheus metrics exporter for GoCache. Hooks every command via a post-hook, tracks per-command counters and latency histograms, and serves a `/metrics` HTTP endpoint in Prometheus text exposition format.

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
       gobservability:
         critical: false
         priority: 100
         scopes: ["hook:post"]
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
| HTTP port | `GOBSERVABILITY_PORT` env var | `:9100` | Address for the metrics HTTP server |
| Critical | `gocache.yaml` override | `false` | Plugin crash does not affect the server |
| Priority | `gocache.yaml` override | `100` | Low priority -- runs after all other hooks |
| Scopes | `gocache.yaml` override | `["hook:post"]` | Only needs post-hook access |

## Metrics

### `gocache_commands_total` (counter)

Total number of commands processed, labeled by command name.

```
gocache_commands_total{command="SET"} 1234
gocache_commands_total{command="GET"} 5678
```

### `gocache_command_errors_total` (counter)

Total number of commands that returned an error, labeled by command name.

```
gocache_command_errors_total{command="SET"} 2
```

### `gocache_command_duration_seconds` (histogram)

Command execution latency in seconds, labeled by command name. Uses server-measured timing via the `_elapsed_ns` hook context value.

Bucket boundaries: 1ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s.

```
gocache_command_duration_seconds_bucket{command="SET",le="0.001"} 100
gocache_command_duration_seconds_bucket{command="SET",le="0.005"} 200
...
gocache_command_duration_seconds_bucket{command="SET",le="+Inf"} 1234
gocache_command_duration_seconds_sum{command="SET"} 12.34
gocache_command_duration_seconds_count{command="SET"} 1234
```

### `gocache_plugin_info` (gauge)

Plugin metadata.

```
gocache_plugin_info{name="gobservability",version="0.1.0"} 1
```

## How It Works

The plugin implements `HookPlugin` with a wildcard post-hook (`pattern: "*"`). After every command execution, the server sends a `HookRequestV1` with:

- Command name and arguments
- Result value and error string
- Hook context including `_elapsed_ns` (server-measured command execution time in nanoseconds)

The plugin records the command name, duration, and error status in a thread-safe in-memory collector. The HTTP handler renders the accumulated metrics in Prometheus text format on demand.

No external dependencies -- the Prometheus text format is written directly. The plugin binary is self-contained.

## Design Diagrams

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [Architecture](design/component/components_gobservability.puml) | Plugin internal structure |
| Sequence | [Metrics Collection](design/sequence/sequence_metrics_collection.puml) | Data flow from command to /metrics |
