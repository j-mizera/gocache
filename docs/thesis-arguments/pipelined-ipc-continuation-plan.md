# Continuation plan: pipelined IPC observability optimization

## Context

ADR-0028 / Phase 2A improved event interest gating and reduced per-plugin event projection cost. The heavy benchmark results show that this helped, especially for IPC pipelined throughput compared with the previous GoCache baseline. However, GoCache IPC observability remains far behind Valkey under pipelining.

Evidence anchor: `bench/results/heavy-event-hotpath-20260530/summary.md`.

## Current conclusion

The current PR should be treated as a successful checkpoint for producer-side observability gating and measurement coverage. Further pipelined IPC improvements should be a continuation PR, not folded into this checkpoint.

## Continuation implementation result

The first continuation branch implemented normal-frame GCPC batching, lazy fire-and-forget envelope construction, per-plugin outbound writer statistics, and a bounded 200 µs telemetry-only batch collection window. Evidence is stored in `bench/results/pipelined-ipc-observability-20260530/summary.md`.

Result: delayed batching improved the IPC Prometheus path versus the PR #89 IPC anchor (`+13.89%` pipelined geometric-mean RPS, `-6.07%` p99) and versus the first no-delay continuation capture (`+20.66%` pipelined RPS, `-10.48%` p99). It does **not** close the thesis-visible pipelined IPC gap: delayed IPC full remains `-51.35%` geometric-mean RPS versus Valkey and `-42.19%` versus the current GoCache core capture in pipelined mode.

Updated conclusion: lower-level frame batching is useful but insufficient. The next optimization should focus on reducing per-command event volume for metrics-only consumers, coarser Prometheus aggregation, and cheaper event projection before considering protocol-level batch messages or stream-topology work.

## Planned continuation PR scope

### 1. Batch GCPC event delivery

Problem:

- The event bridge still sends one GCPC envelope per event.
- Under pipelining, per-command event delivery amplifies IPC overhead.

Planned direction:

- Add a bounded per-plugin event queue with batch flush thresholds.
- Flush by max batch size and max latency budget.
- Preserve ordering within a plugin stream.
- Measure dropped/queued/batched event counters.

Expected benefit:

- Fewer IPC writes and fewer protobuf framing operations per command batch.

### 2. Aggregate Prometheus metrics before IPC

Problem:

- Prometheus does not need a full `command.completed` event for every command if it only records counters and histograms.

Planned direction:

- Introduce compact metric delta messages or server-side aggregation snapshots.
- Let Prometheus consume aggregated counters/histograms instead of full per-command events when possible.
- Keep full event stream available for plugins that require exact event records.

Expected benefit:

- Lower event volume for metrics-only observability.

### 3. Late protobuf materialization and pooled encoding

Problem:

- Some payloads are still materialized earlier than necessary.
- Encoding and frame allocation remain expensive in IPC paths.

Planned direction:

- Delay protobuf construction until after subscription and plugin visibility checks.
- Reuse transient buffers where ownership is clear.
- Avoid pooling request-owned operation/context state.

Expected benefit:

- Lower allocation rate and GC pressure in observability paths.

### 4. Core pipelined command path improvements

Problem:

- Core pipelined mode is partially competitive but loses to Valkey overall, especially on collection-heavy commands.

Planned direction:

- Profile `HSET`, `SADD`, `RPUSH`, `LPOP`, `MSET`, and `LRANGE_100` under pipelined load.
- Inspect RESP result serialization and writer batching.
- Inspect shard lock behavior and allocation hotspots.

Expected benefit:

- Better pipelined core throughput and latency independent of IPC plugins.

### 5. Memory/RSS attribution

Problem:

- GoCache RSS is much higher than Valkey, and the current implementation increased core RSS in the benchmark run.

Planned direction:

- Use pprof heap profiles and runtime metrics under the same benchmark suite.
- Attribute RSS growth to cache data, operation/event structures, protobuf buffers, runtime heap, and plugin machinery.
- Decide whether reductions fit the safety goals or would require unsafe/manual-memory trade-offs outside thesis scope.

Expected benefit:

- Clearer thesis explanation and actionable allocation targets.

## Non-goals for continuation PR

- Do not remove operation/event observability to match Valkey memory numbers.
- Do not use unsafe Go without a separate architecture decision.
- Do not weaken plugin isolation by importing `pkg/` from plugins.
- Do not claim Valkey parity for pipelined IPC until measured.

## Suggested verification for next PR

1. Package benchmarks with `-benchmem` for event bridge, manager, transport, and Prometheus plugin.
2. Heavy Docker benchmark in `bench/redis-benchmark`:
   - core GoCache standard + pipelined;
   - IPC GoCache standard + pipelined;
   - Valkey reference.
3. Compare against `bench/results/heavy-event-hotpath-20260530/`.
4. Run `go test ./...`, `go vet ./...`, `go test -race ./api/... ./commons/... ./sdk/... ./plugins/... ./pkg/plugin/...`, and `scripts/check-plugin-isolation.sh`.

## PR framing

Proposed next PR title:

> Continue ADR-0028: batch IPC observability for pipelined workloads

Proposed PR description focus:

- Continuation of producer-side observability optimization.
- Target: pipelined IPC Prometheus overhead.
- Baseline: `bench/results/heavy-event-hotpath-20260530/`.
- Success criteria: improve IPC pipelined geometric mean throughput and p99 without regressing standard core throughput or plugin isolation.
