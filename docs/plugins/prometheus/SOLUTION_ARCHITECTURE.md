---
title: Prometheus Plugin Architecture
description: Design decisions for the metrics exporter — event-only metrics, IPC plugin, in-memory aggregation, /metrics serving
status: living
last_updated: 2026-05-30
related:
  - Plugins
  - ADR-0023
  - ADR-0028
---

# Prometheus Plugin — Solution Architecture

## Overview

The Prometheus plugin is a non-critical GoCache IPC plugin that collects command-level metrics and exposes them through a Prometheus-compatible HTTP endpoint. It runs as a separate OS process and receives runtime metric samples through GCPC event delivery.

## Design Decisions

### Event-only metrics

The current plugin subscribes only to command-completion events. ADR-0028 changes the target taxonomy from hook-phase `command.post` to cheap `operation.completed` summaries. This means:

- Metrics collection is off the blocking command hook path.
- The plugin cannot deny, mutate, or enrich commands.
- The server should pay optional payload construction cost only when the aggregate interest mask says a subscriber requested command-completion summary fields.
- The permission surface is limited to `events` and health/readiness query scopes.

### Server-measured latency

The current prototype reads `CommandPostEventV1.elapsed_ns`. Under ADR-0028, the plugin should read equivalent server-measured duration from `operation.completed` summary fields, not from plugin-side IPC timing.

### No OTLP responsibilities

The Prometheus plugin owns pull-based metrics only. Runtime OTLP traces/logs/events belong in a separate `instrumentation` plugin so metrics scraping and trace export can have different batching, sampling, and backpressure policies.

### No external Prometheus dependency

The Prometheus text exposition format is rendered directly. Avoiding `prometheus/client_golang` keeps the binary small and avoids a large transitive dependency tree.

### Thread-safe collector

The metrics collector uses a single mutex protecting a map of per-command statistics. This is acceptable because:

- The collector is only accessed from event handler goroutines and the HTTP handler.
- Event delivery already happens in the plugin process, outside the cache hot path.
- The HTTP handler snapshots under lock and renders after unlocking.

## Data Flow

### Command to Metric

1. Server executes the command and measures elapsed time.
2. The aggregate interest mask indicates whether a Prometheus-style subscriber wants cheap command-completion summary fields.
3. Server emits the requested command-completion summary (`command.post` today; `operation.completed` target under ADR-0028) without constructing rich args/context/log payloads.
4. Plugin ignores every non-command-completion record.
5. Plugin records command, duration, and error status into the collector.

### Prometheus Scrape

1. Prometheus requests `/metrics`.
2. The HTTP handler snapshots collector state under lock.
3. The handler renders counters, error counters, histograms, and plugin info in Prometheus text format.

## Metrics Data Model

Each command name maps to a statistics struct containing:

| Field | Type | Description |
|-------|------|-------------|
| total | uint64 | Total invocations |
| errors | uint64 | Invocations that returned an error |
| sum | float64 | Sum of all durations in seconds |
| counts | []uint64 | Per-bucket histogram counts |

Histogram bucket boundaries (seconds): 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0.

## Plugin Interfaces

| Interface | Implementation |
|-----------|---------------|
| `Plugin` | Name: "prometheus", Version: project version, Critical: false |
| `EventPlugin` | EventTypes: command-completion summary (`command.post` today; `operation.completed` target under ADR-0028) |
| `QueryPlugin` | `/healthz` and `/readyz` server queries |
| `ScopePlugin` | Scopes: [`events`, `server:query:health`, `server:query:plugins`] |

## Failure Mode

The plugin is non-critical. If it crashes:

- The server continues operating normally.
- The plugin manager restarts it up to `max_restarts`.
- Command execution is not denied or mutated.
- Metrics are lost on restart because aggregation is in memory.

## Design Diagrams

Architecture belongs in the PlantUML diagrams rather than inline prose diagrams.

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [Architecture](design/component/components_prometheus.puml) | Plugin internal structure and data flow |
| Sequence | [Metrics Collection](design/sequence/sequence_metrics_collection.puml) | End-to-end flow from command-completion telemetry to scrape |
