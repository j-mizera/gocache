# Gobservability -- Solution Architecture

## Overview

Gobservability is a non-critical GoCache plugin that collects command-level metrics and exposes them via a Prometheus-compatible HTTP endpoint. It runs as a separate OS process, communicating with the server over Unix domain sockets using the GCPC protocol.

## Design Decisions

### Post-hook only

The plugin uses a single wildcard post-hook (`pattern: "*", phase: POST`). This means:

- It fires after every command completes, with access to the result and execution timing.
- It never blocks command execution (non-critical hooks are fire-and-forget).
- It does not need pre-hook scope, minimizing its permission surface.

### Server-measured latency

The plugin reads `_elapsed_ns` from the hook context rather than measuring time itself. This gives accurate server-side command execution time, not IPC round-trip time. The server injects this value into the hook context before dispatching post-hooks.

### No external dependencies

The Prometheus text exposition format is simple enough to render directly. Avoiding `prometheus/client_golang` keeps the binary small (~14MB) and eliminates a large transitive dependency tree. If the format needs evolve (e.g., OpenMetrics), the hand-written renderer can be swapped without API changes.

### Thread-safe collector

The metrics collector uses a single mutex protecting a map of per-command statistics. This is acceptable because:

- The collector is only accessed from hook handler goroutines and the HTTP handler.
- Lock contention is low -- hook handlers write one entry, HTTP handler reads a snapshot.
- The collector is not on the cache hot path (it's in a separate process).

## Component Architecture

```
                      GoCache Server
                           |
                     Unix Domain Socket
                     (GCPC Protocol)
                           |
              +------------v-------------+
              |   Gobservability Plugin  |
              |                          |
              |  +--------------------+  |
              |  |   Hook Handler     |  |
              |  | HandleHook(req)    |  |
              |  | reads _elapsed_ns  |  |
              |  +--------+-----------+  |
              |           |              |
              |  +--------v-----------+  |
              |  |    Collector       |  |
              |  | counters + histos  |  |
              |  | (mutex-protected)  |  |
              |  +--------+-----------+  |
              |           |              |
              |  +--------v-----------+  |
              |  |   HTTP Server      |  |
              |  | :9100/metrics      |  |
              |  | Prometheus format  |  |
              |  +--------------------+  |
              +--------------------------+
                           |
                      Prometheus
                      (scrapes /metrics)
```

## Data Flow

### Command to Metric

```
Client sends SET key value
    -> Server executes command, measures elapsed time
    -> Server sends HookRequestV1 (POST) to gobservability
        context: {_start_ns, _elapsed_ns}
        result_value: "OK"
        result_error: ""
    -> Plugin HandleHook():
        1. Parse _elapsed_ns from context
        2. Determine isError from result_error
        3. collector.Record("SET", elapsedNs, isError)
            - Increment total counter for "SET"
            - Increment error counter if isError
            - Place duration into histogram bucket
            - Add to sum
```

### Prometheus Scrape

```
Prometheus GET /metrics
    -> HTTP handler calls collector.WritePrometheus()
        1. Lock mutex, snapshot all stats
        2. Unlock mutex
        3. Render counters (gocache_commands_total)
        4. Render error counters (gocache_command_errors_total)
        5. Render histograms with cumulative buckets
        6. Render plugin info gauge
    -> Return text/plain response
```

## Metrics Data Model

Each command name maps to a statistics struct containing:

| Field | Type | Description |
|-------|------|-------------|
| total | uint64 | Total invocations |
| errors | uint64 | Invocations that returned an error |
| sum | float64 | Sum of all durations in seconds |
| counts | []uint64 | Per-bucket histogram counts (10 entries: 9 boundaries + Inf) |

Histogram bucket boundaries (seconds): 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0.

Prometheus histograms use cumulative counts -- each bucket includes all observations less than or equal to its boundary. The renderer accumulates counts when writing.

## Plugin Interfaces

| Interface | Implementation |
|-----------|---------------|
| `Plugin` | Name: "gobservability", Version: "0.1.0", Critical: false |
| `HookPlugin` | Hooks: [{Pattern: "*", Phase: POST}] |
| `ScopePlugin` | Scopes: ["hook:post"] |

## Failure Mode

The plugin is non-critical. If it crashes:
- The server continues operating normally.
- The plugin manager restarts it (up to `max_restarts`).
- No command execution is blocked or delayed.
- Metrics are lost on restart (in-memory only).

Hook dispatch is fire-and-forget for non-critical plugins -- the server does not wait for a response.

## Design Diagrams

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [Architecture](design/component/components_gobservability.puml) | Plugin internal structure and data flow |
| Sequence | [Metrics Collection](design/sequence/sequence_metrics_collection.puml) | End-to-end flow from command to scrape |
