---
title: Prometheus Plugin Roadmap
description: Phase tracker for the Prometheus plugin — counters, histograms, info gauge, server-measured latency
status: living
last_updated: 2026-05-30
related:
  - Plugins
  - ADR-0023
  - ADR-0028
---

# Prometheus Plugin Roadmap

## v0.1 — Complete

- Async `command.post` event subscription for all commands
- Per-command counters (`gocache_commands_total`, `gocache_command_errors_total`)
- Per-command latency histograms (`gocache_command_duration_seconds`, 9 buckets)
- Plugin info gauge
- Prometheus text exposition format on HTTP `/metrics`
- `/healthz` and `/readyz` via server query scopes
- No command hooks, operation hooks, or OTLP exporter responsibilities

## v0.2 — Planned

- Move metrics input from hook-phase `command.post` naming to ADR-0028 `operation.completed` summaries.
- Request only the interest-mask detail needed for low-cardinality counters and histograms.
- Connection gauge (`gocache_connections_active`) via connection events
- Memory usage gauge (`gocache_memory_used_bytes`) via periodic cache stats query
- Key count gauge (`gocache_keys_total`)
- Hit/miss ratio counters for GET commands if the event schema exposes cache hit/miss state
- Configurable histogram buckets via environment variable

## Out of Scope

- Runtime OTLP traces/logs/events export. A separate `instrumentation` plugin owns that responsibility.
- Denial or enrichment behavior. Those remain command/operation hook responsibilities for other plugins.
