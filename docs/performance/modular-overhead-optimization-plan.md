---
title: Modular Overhead Optimization Plan
description: Benchmark-gated plan for reducing IPC, runtime instrumentation, lifecycle OTLP, and Pub/Sub overhead while preserving plugin isolation
status: active
last_updated: 2026-05-30
related:
  - ADR-0006-builtin-vs-third-party-transport
  - ADR-0020-client-push-via-gcpc
  - ADR-0022-modular-performance-budget
  - ADR-0028-operation-observability-and-log-records
---

# Modular Overhead Optimization Plan

This plan captures the post-baseline work for GoCache's plugin microkernel path. The target is not merely to improve the current IPC/runtime-instrumentation/lifecycle-OTLP/Pub/Sub numbers; the acceptance gate is **<=20% throughput regression** for modular hot-path features, with correctness and p99 latency still passing.

## Baseline evidence

Captured under `bench/results/bench-discovered-fixes-baseline/`.

### Request/response IPC and former runtime OTLP instrumentation

| Comparison | Evidence | Current verdict |
|---|---|---|
| Core GoCache -> `gocache-ipc` | Standard suite roughly 53-64% slower; pipelined suite roughly 82-97% slower | Fails the modular budget |
| `gocache-ipc` -> former `gocache-ipc-otel` | Standard overhead roughly 12-23%; pipelined overhead roughly 17-22% | Historical evidence for why OTLP export was split out of Prometheus |
| Core GoCache -> former `gocache-ipc-otel` | `SET` 131,234 rps -> 41,102 rps; `GET` 128,535 rps -> 39,857 rps | Historical failure of the combined observability plugin shape |

### Pub/Sub fanout

| Scenario | Valkey rps | GoCache Pub/Sub rps | Delta | Delivery |
|---|---:|---:|---:|---|
| `PUBLISH_fanout_0` | 42,718 | 15,814 | -63.0% | ok |
| `PUBLISH_fanout_1` | 17,854 | 9,836 | -44.9% | ok |
| `PUBLISH_fanout_10` | 2,060 | 1,646 | -20.1% | ok |

Fanout10 is close to the acceptance gate; fanout0/fanout1 are not. That shape suggests fixed IPC/command overhead dominates low-fanout Pub/Sub, while per-subscriber fanout cost is closer to acceptable once enough push work is amortized.

## Design constraints

- Preserve ADR-0006's separation: embedded plugins are for startup-critical or budget-failing built-ins; IPC plugins remain the default isolation boundary for runtime extensions.
- Preserve ADR-0020's `ClientPushV1` as the generic push primitive. Optimize or specialize the data-plane only where benchmarks prove the generic path cannot meet budget.
- Do not replace local GCPC with gRPC, Connect, NATS, or Watermill for the hot path. Those libraries are candidates for future distributed integrations, not the first local IPC fix.
- Do not retry known-negative core experiments as first fixes: cache-wide read-lock bypass and broad long-lived buffer pools both have prior negative evidence.
- Only engine memory writes, Redis atomicity boundaries, persistence ordering, and explicitly critical hooks should synchronously block command responses. Telemetry and best-effort fanout should use bounded async paths.

## Implementation sequence

### Phase 1: Per-plugin FIFO writer loop

**Target files**: `pkg/plugin/router/router.go`, `pkg/plugin/cmdhooks/executor.go`, `pkg/plugin/ophooks/executor.go`.

**Change**:

- Add one bounded outbound queue and one writer goroutine per plugin connection.
- Route every send through that queue so protobuf writes to a connection remain serialized.
- Remove goroutine-per-send wrappers from command hooks and operation hooks after the writer loop owns async delivery.
- Preserve backpressure semantics: critical sends must either enqueue or return an explicit error; noncritical observe-only sends may drop with counters once Phase 2 exists.

**Tests/gates**:

- Unit/integration coverage for FIFO ordering, bounded queue behavior, plugin disconnect cleanup, and no send-after-close panic.
- `go test -race ./pkg/plugin/manager ./plugins/prometheus ./sdk/pluginsdk`.
- Re-run `./bench/redis-benchmark/run-matrix.sh <label>` and compare core -> IPC Prometheus.

### Phase 2: Event-only runtime instrumentation

**Target files**: `api/events`, `pkg/events`, `pkg/pipeline`, `pkg/plugin/manager`, `plugins/prometheus`, and future `plugins/instrumentation` packages.

**Change**:

- Keep denial/enrichment hooks on the blocking path.
- Move runtime telemetry out of command hooks and operation hooks into typed asynchronous events.
- Keep `prometheus` pull-metrics-only; future `instrumentation` owns OTLP traces/logs/events.
- Surface dropped runtime telemetry events as metrics/logs instead of silently hiding overload.

**Tests/gates**:

- Critical hook failures still obey their configured failure policy.
- Runtime telemetry event overload never blocks normal command responses beyond the enqueue attempt.
- `gocache-ipc` with Prometheus must move inside the <=20% overhead budget or produce a profile explaining the remaining fixed cost.
- Lifecycle OTLP remains limited to pre-IPC startup/failure/shutdown spans and is not used for runtime operation instrumentation.

### Phase 2A: Event/log producer attribution and lazy projection gate

**Target files**: `api/operations`, `api/events`, `pkg/pipeline`, `pkg/events`, `pkg/plugin/manager`, `pkg/plugin/router`, `commons/transport`, `pkg/logcollector`, `plugins/prometheus`.

**Change**:

- Preserve the existing docker attribution modes: `full`, `events-off`, and `bridge-off`.
- Add package-level `go test -bench -benchmem` coverage before more transport topology work.
- Attribute operation creation/enrichment/context snapshots, event constructor allocation, no-sink versus full-sink pipeline cost, event bus fanout, bridge clone/filter/envelope cost, plugin queue enqueue/write cost, protobuf marshal/framing cost, log JSON parse/log-entry injection cost, and Prometheus aggregation cost.
- Move toward ADR-0028's producer model only after attribution exists: compact operation/command summaries first, then subscriber-specific event/log/protobuf projection.
- Validate the context representation explicitly: immutable request snapshot, fixed typed field array, bitset/flag interest checks, lazy extension map/overlay, and per-sink projection cache.
- Treat `log.entry` as a prototype artifact for benchmark purposes. The target log path is a bounded async `LogRecord` diagnostics lane with level/component interest masks, sampling, batching, and drop counters.

**Suggested package benchmark coverage**:

| Package | Benchmark focus |
|---|---|
| `api/operations` | operation construction, enrichment, raw/redacted context snapshots |
| `api/events` | command/operation/log event constructors and operation ID enrichment |
| `pkg/pipeline` | no-sink, full-sink, tracker-only, event-only, hook-only, and sink-gate paths |
| `pkg/events` | bus emit with no subscribers, one subscriber, and many subscribers |
| `pkg/plugin/manager` | subscriber-specific projection or current clone/filter/envelope path |
| `pkg/plugin/router` | enqueue, fire-and-forget send, writer-loop latency and drops |
| `commons/transport` | protobuf marshal/unmarshal and frame send/receive allocations |
| `pkg/logcollector` | current JSON parse/plain-text parse/log-entry injection baseline before removal |
| `plugins/prometheus` | metric record/update and exposition write costs |

**Tests/gates**:

- Each suspected hotspot must have a focused benchmark before an optimization is accepted.
- `benchmem` must identify whether the win is allocation reduction, byte reduction, or pure CPU/lock reduction.
- Pooling proposals must prove buffer/encoder reuse only; request-owned operation/context state must not be pooled across async projections or subscribers.
- Re-run the IPC benchmark matrix with labels for `full`, `events-off`, and `bridge-off`; do not compare incompatible harnesses as the same claim.
- Capture CPU/heap/mutex/block profiles only after benchmark modes identify the stage under test.
- Track `plugin.ipc` queue-full drops, accepted/drop totals, enqueue latency, write latency, and lag for every docker capture.
- Do not move yamux or stream topology ahead of this phase unless measurements show producer construction is no longer the dominant remaining deficit.

### Phase 3: GCPC stream topology evaluation

**Target files**: `api/gcpc/v1`, `pkg/plugin/manager`, `pkg/plugin/router`, `sdk/pluginsdk`.

**Change**:

- Benchmark whether one UDS stream per plugin remains sufficient after Phase 1/2.
- Compare a single FIFO writer against multiple Unix-domain socket streams per plugin, split by traffic class: command RPC, hooks/operation hooks, events/server-query/control, and client push where relevant.
- Compare multi-UDS streams against a single-UDS priority queue before accepting the lifecycle complexity of multiple sockets.
- Define per-stream ordering guarantees explicitly. Operation replay and same-command request/response ordering must remain FIFO within their stream; cross-stream ordering must not be assumed unless the protocol adds an explicit barrier.

**Tests/gates**:

- Multi-stream plugins must fail registration if any required stream is missing.
- Plugin shutdown must close every stream without goroutine leaks.
- Benchmarks must show enough head-of-line blocking reduction to justify extra handshake and lifecycle surface.

### Phase 4: GCPC allocation and correlation cleanup

**Target files**: `api/gcpc/v1`, `commons/transport`, `pkg/plugin/router`, `sdk/pluginsdk`.

**Change**:

- Use numeric envelope/request IDs on the hot path instead of string correlation where protocol compatibility allows it.
- Consider `proto.MarshalOptions.MarshalAppend` and scoped byte-slice reuse for per-message allocations.
- Keep buffer pooling narrow and measurement-driven; do not introduce broad `sync.Pool` churn without benchmark proof.

**Tests/gates**:

- Protocol compatibility tests for old/new plugins if wire fields change.
- Allocation benchmarks before and after the change.
- Full IPC matrix rerun.

### Phase 5: Pub/Sub push batching or data-plane specialization

**Target files**: `pkg/plugin/manager`, `plugins/pubsub`, possibly `api/gcpc/v1` for batch message shape.

**Change**:

- Batch or group `ClientPushV1` writes when one publish produces multiple pushes.
- Avoid per-subscriber flushes where a connection-local queue can preserve order.
- If fanout0/fanout1 still fail <=20% after batching, introduce a core or embedded Pub/Sub data-plane for the Redis-compatible built-in while leaving `ClientPushV1` available for generic plugin push use cases.

**Tests/gates**:

- Real subscriber delivery verification remains exact: every published message reaches every subscriber once.
- `PUBLISH_fanout_0`, `PUBLISH_fanout_1`, and `PUBLISH_fanout_10` all meet the budget or have an explicit ADR update explaining why a specialized path is required.

## Verification loop

For every phase:

1. Run targeted Go tests and race tests for touched plugin packages.
2. Run `go test ./...` and `go vet ./...`.
3. Re-run the relevant benchmark harness:
   - Request/response: `./bench/redis-benchmark/run-matrix.sh <label>`.
   - Pub/Sub: `./bench/redis-benchmark/run-pubsub.sh <label> --target valkey` and `./bench/redis-benchmark/run-pubsub.sh <label> --target gocache-pubsub`.
4. Compare against `bench/results/bench-discovered-fixes-baseline/` and record the result directory in the PR/ADR update.

## Stop conditions

Stop and re-plan before coding further if:

- A phase improves throughput but loses delivery correctness or introduces race detector failures.
- Memory/RSS grows enough to erase the benefit of the throughput win.
- The fix requires changing plugin semantics rather than just the transport/hook delivery shape.
- A proposed optimization repeats a previously rejected experiment without new evidence that the underlying runtime or workload changed.
