# ADR-0036: Batch-Level Pipelined Operation Telemetry

## Status

accepted

## Context

Pipelined execution currently relies on per-command telemetry scopes, but the slot tracker is sharded per connection and hot pipelined connections can exhaust shard slots before the sequential drain worker recycles completed slots. In the 2026-06-09 telemetry-loss verification run (`BENCH_STATS=1 BENCH_PPROF=1 BENCH_N=100000 BENCH_PIPELINE=10 REBUILD=0 ./bench/redis-benchmark/run-ipc.sh telemetry-loss-verify-20260609 --target gocache-ipc`), the standard benchmark reported `pipeline.evaluations=1500000`, `pipeline.event.operation_started=1500000`, and `pipeline.event.operation_completed=1500000`, but the pipelined run reported only `pipeline.event.operation_started=706210` and `pipeline.event.operation_completed=706210` for the same `pipeline.evaluations=1500000`.

That leaves 793790 command-level telemetry scopes untracked (~52.9%). The focused run proves command-level lifecycle emission loss under pipelined load. `operation_tracker.skipped_operations=0` does not by itself explain the missing lifecycle records; it only means this run did not demonstrate the slot-acquisition skip-counter path. Prior code investigation identified per-connection shard pressure and sequential drain recycling as the working hypothesis/mechanism to verify with always-on per-shard pressure metrics during implementation.

## Decision

In pipelined mode, we record one operation lifecycle per pipeline batch/runBatch instead of one per command. The batch operation carries aggregate metadata: command count, pipeline size/depth, connection reference, duration, aggregate success/error facts, and optional cheap command-name/class counts only if they do not reintroduce hot-path allocations.

Non-pipelined commands remain per-command telemetry. No per-command pipelined fallback, debug flag, or backward-compatibility mode is required.

## Measurement Results (T-MEASURE)

T-MEASURE re-baselined the decision on `perf/telemetry-processing` at commit `f9c3863` after T-BATCH + T-GATE.

- Standard mode (`P=1`): IPC `SET` 111,731 RPS vs OTel `SET` 112,739 RPS. OTel overhead is effectively 0%, with p99 latency in the ~0.38–0.48 ms range across commands.
- Pipelined mode (`P=10`): IPC `SET` 854,700 RPS vs OTel `SET` 854,700 RPS; IPC `GET` 934,579 RPS vs OTel `GET` 1,010,101 RPS.
- Skip counters stayed in the low-50% range even after the 5× operation-count reduction: IPC `skipped_operations=165,551`, `operation_completed=134,651`, `total=300,202` (~55% skipped); OTel `skipped_operations=156,630`, `operation_completed=143,576`, `total=300,206` (~52% skipped).
- RSS stayed materially unchanged relative to the pre-fix shape: IPC `delta_rss=232MB` (61MB → 293MB); OTel `delta_rss=205MB` (63MB → 268MB).
- The structural bottleneck is still the single sequential drain worker; per-batch telemetry is correct and accepted, but T-PARALLEL remains justified as a complementary follow-up because batch-level telemetry alone does not eliminate sustained per-shard exhaustion.

## Alternatives Considered

### Per-command telemetry with parallelized per-shard drain workers

- **Pros**: Can complement batch-level telemetry by reducing shard-slot pressure under hot pipelined load while preserving command-level granularity.
- **Cons**: Adds more concurrent drain machinery.
- **Status after T-MEASURE**: This is no longer a rejected alternative; the measurements justify it as T-PARALLEL, a follow-up needed to address the still-high skip rate under sustained pipelined load.

### Dynamic hot-shard slot growth / control loop

- **Pros**: Keeps the per-command model while trying to avoid slot exhaustion.
- **Cons**: Introduces feedback control complexity and unpredictable memory/latency behavior.
- **Why not**: Too much machinery for a problem that is specific to pipelined batch observability.

### Per-command debug/env fallback

- **Pros**: Could preserve detail when explicitly enabled.
- **Cons**: Adds a new mode and a silent split between default and debug observability.
- **Why not**: The decision explicitly does not require a fallback or compatibility flag.

### Accept silent telemetry skips because commands still execute

- **Pros**: No implementation work.
- **Cons**: Makes telemetry incorrect by design.
- **Why not**: Silent skips are not acceptable for operation observability.

## Consequences

### Positive

- Batch telemetry matches the actual unit of work in pipelined execution.
- The hot path no longer depends on per-command slot availability in pipelined mode.
- Aggregate metrics still capture the important batch-level facts without forcing per-command bookkeeping.

### Negative

- Per-command visibility inside a pipeline is reduced unless derived from aggregate metadata.
- Consumers that expect one telemetry record per command must treat pipelined batches as a different unit of observation.

### Risks

- **Risk**: Aggregate metadata reintroduces hot-path allocations or extra work. **Mitigation**: Keep the batch record cheap, prefer counts over per-command materialization, and verify with the same benchstats/pprof workflow used for this decision.
- **Risk**: Mixed pipelined and non-pipelined telemetry can be misread as inconsistent. **Mitigation**: Document the unit-of-work distinction clearly in code and downstream analysis notes.

## Related

- ADR-0013: Pipeline batch coalescing
- ADR-0028: Operation Observability and Log Records
- ADR-0029: OperationTracker Sidecar for Low-Overhead Telemetry
- ADR-0033: Common OperationTracker Sharding Contract
- ADR-0035: No-sink fast-path decision — always-on telemetry with measured fallback
- `docs/performance/README.md`
