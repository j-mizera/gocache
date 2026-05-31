---
title: Prometheus Plugin Architecture
description: Design decisions for the metrics exporter — pull-based command metrics, IPC plugin, in-memory rendering, /metrics serving
status: living
last_updated: 2026-05-30
related:
  - Plugins
  - ADR-0023
  - ADR-0028
---

# Prometheus Plugin — Solution Architecture

## Overview

The Prometheus plugin is a non-critical GoCache IPC plugin that exposes core command-dispatch metrics through a Prometheus-compatible HTTP endpoint. It runs as a separate OS process and pulls aggregate command counters/histograms through the generic GCPC server-query topic `metrics.commands` during scrape.

## Design Decisions

### Pull-based command metrics

The plugin requests `server:query:metrics.commands` instead of subscribing to command-completion events. This means:

- Metrics collection is off the blocking command hook path.
- The plugin cannot deny, mutate, or enrich commands.
- The server records compact command aggregates only while a metrics-query consumer is registered.
- The permission surface is limited to command-metrics and health/readiness query scopes.

### Server-measured latency

The server-side command metrics collector records elapsed nanoseconds measured around command execution. The plugin receives aggregate nanosecond sums and bucket counts through `metrics.commands`; it does not infer latency from plugin-side IPC timing.

### No OTLP responsibilities

The Prometheus plugin owns pull-based metrics only. Runtime OTLP traces/logs/events belong in a separate `instrumentation` plugin so metrics scraping and trace export can have different batching, sampling, and backpressure policies.

### No external Prometheus dependency

The Prometheus text exposition format is rendered directly. Avoiding `prometheus/client_golang` keeps the binary small and avoids a large transitive dependency tree.

### Thread-safe collector

The metrics collector uses a single mutex protecting a map of per-command statistics. This is acceptable because:

- The local collector is updated by the HTTP handler from a server-query snapshot.
- Server-side aggregation is guarded by an active-consumer check so deployments without a metrics query consumer skip recording.
- The HTTP handler snapshots under lock and renders after unlocking.

## Data Flow

### Command to Metric

1. Server executes the command and measures elapsed time.
2. If at least one plugin has `server:query:metrics.commands`, the core command pipeline records command name, elapsed nanoseconds, and error flag into the server-side command metrics collector.
3. No command event protobuf or per-plugin event projection is needed for Prometheus metrics.

### Prometheus Scrape

1. Prometheus requests `/metrics`.
2. The HTTP handler calls `QueryServer("metrics.commands")` through the SDK session.
3. The server validates `server:query:metrics.commands` and returns aggregate counters, error counts, nanosecond sums, and histogram bucket counts.
4. The plugin replaces its local collector snapshot and renders counters, error counters, histograms, and plugin info in Prometheus text format.

## Metrics Data Model

Each command name maps to a statistics struct containing:

| Field | Type | Description |
|-------|------|-------------|
| total | uint64 | Total invocations |
| errors | uint64 | Invocations that returned an error |
| sum | float64 | Sum of all durations in seconds after converting the server's nanosecond sum |
| counts | []uint64 | Per-bucket histogram counts |

Histogram bucket boundaries (seconds): 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0.

## Plugin Interfaces

| Interface | Implementation |
|-----------|---------------|
| `Plugin` | Name: "prometheus", Version: project version, Critical: false |
| `QueryPlugin` | `/metrics`, `/healthz`, and `/readyz` server queries |
| `ScopePlugin` | Scopes: [`server:query:metrics.commands`, `server:query:health`, `server:query:plugins`] |

## Failure Mode

The plugin is non-critical. If it crashes:

- The server continues operating normally.
- The plugin manager restarts it up to `max_restarts`.
- Command execution is not denied or mutated.
- Local rendered metrics are refreshed on the next successful scrape from the server-side aggregate snapshot.

## Design Diagrams

Architecture belongs in the PlantUML diagrams rather than inline prose diagrams.

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [Architecture](design/component/components_prometheus.puml) | Plugin internal structure and data flow |
| Sequence | [Metrics Collection](design/sequence/sequence_metrics_collection.puml) | End-to-end flow from command metrics aggregation to scrape |
