# Gobservability Roadmap

## v0.1 -- COMPLETE

- Post-hook on all commands (`*`)
- Per-command counters (`gocache_commands_total`, `gocache_command_errors_total`)
- Per-command latency histograms (`gocache_command_duration_seconds`, 9 buckets)
- Plugin info gauge
- Prometheus text exposition format on HTTP `/metrics`
- Server-measured latency via `_elapsed_ns` hook context
- No external dependencies

## v0.2 -- Planned

- Connection gauge (`gocache_connections_active`) via HELLO/QUIT hook tracking
- Memory usage gauge (`gocache_memory_used_bytes`) via periodic cache stats query (requires plugin-to-server read-back)
- Key count gauge (`gocache_keys_total`)
- Hit/miss ratio counters for GET commands
- Configurable histogram buckets via environment variable

## v0.3 -- Planned (depends on Phase 3: REX META)

- OpenTelemetry trace export (reads `_start_ns` + trace context from REX META)
- Span-per-command with parent trace propagation
- Configurable OTEL exporter endpoint
- Trace sampling configuration

## v0.4 -- Planned

- Grafana dashboard template (JSON)
- Alerting rules template (Prometheus alertmanager)
- Slow command log (commands exceeding configurable latency threshold)
- Per-key-pattern metrics (e.g., `gocache_commands_total{command="SET",key_pattern="user:*"}`)
