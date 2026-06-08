# Telemetry Remediation Verification Report - 2026-06-05

Branch: `zero-allocation-telemetry-subplan-b`
Commit captured by harness: `37240842e4b8bac063c4f826a9dcb240605f8a24`

## Commands Run

- `go test -race ./commons/observability`
- `go test -race ./pkg/server`
- `go test -run '^$' -bench 'Benchmark(SlotTracker|TelemetryRecordSubmit|TelemetryTracker|TelemetryManagerInterfaceGetStartCommand|ShardIndex)' -benchmem -count=3 ./commons/observability`
- `RESULTS_DIR="$PWD/bench/results/telemetry-remediation-verification-20260605" BENCH_REPEAT=3 BENCH_STATS=1 BENCH_PPROF=1 REBUILD=1 ./bench/redis-benchmark/run-ipc.sh verification-20260605 --target gocache-ipc`
- Attempted `gocache-ipc-otel` with the same settings, but the command was interrupted before completion; no OTEL gate claim is made from this pass.

## Race Gates

- `commons/observability`: PASS, `ok gocache/commons/observability 3.041s`.
- `pkg/server`: FAIL. Race detector reports concurrent access in `pkg/server/operation_tracker_drain_worker.go` around `operationTrackerDrainScratch.recordPairs`, `intern`, `foldContextUpdate`, `fieldOrDefaultPairs`, and `cloneStringMap` during `TestIT_TelemetryDrainWorkerMaterializesRuntimeLogFromTCPReadError`. The same run also failed `TestServer_ConnectionLifecycleUsesSeparateTelemetryOperations` with close before open ordering: `[operation.started connection.close operation.completed operation.started connection.open operation.completed]`.

## Submit-Path Microbench Gate

All repeated `commons/observability` submit-path microbenchmarks reported `0 B/op` and `0 allocs/op`. Representative medians from the three repeats:

| Benchmark | Median ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkTelemetryRecordSubmit` | 127.5 | 0 | 0 |
| `BenchmarkTelemetryTrackerInterfaceOperationLifecycle` | 218.5 | 0 | 0 |
| `BenchmarkTelemetryManagerInterfaceGetStartCommand` | 62.61 | 0 | 0 |
| `BenchmarkSlotTrackerStartOperation` | 90.24 | 0 | 0 |
| `BenchmarkSlotTrackerRecordTelemetry` | 24.57 | 0 | 0 |
| `BenchmarkSlotTrackerFinishOperation` | 20.77 | 0 | 0 |
| `BenchmarkSlotTrackerInterfaceOperationLifecycle` | 247.3 | 0 | 0 |

## Prometheus IPC Harness

Run: `verification-20260605-gocache-ipc`, `BENCH_REPEAT=3`, `claim_grade=claim-ready`, `BENCH_STATS=1`, `BENCH_PPROF=1`.

- Standard CSV median RPS across commands: 115,207.38.
- Pipelined CSV median RPS across commands: 386,100.38.
- RSS delta: +158,848,778 bytes.
- Pipelined benchstats: `pipeline.evaluations=4500000`, `runtime.gc.heap.allocs.objects=338308636`, `runtime.sync.mutex.wait.total.seconds=525.116394256`.

Normalized against the provided/prometheus-only baseline window of 1,500,000 evaluations (`102.475394096s` mutex wait and `93,664,027` alloc objects):

- Current mutex wait per 1.5M evals: ~175.04s, worse than 102.48s baseline (+70.8%).
- Current alloc objects per 1.5M evals: ~112.77M, worse than 93.66M baseline (+20.4%).

## Mutex/Profile Gate

G10 is not met for the Prometheus IPC diagnostic profile: submit/telemetry frames appear in the mutex diff top path. `mutex/top10-mutex-diff.txt` includes:

- `gocache/commons/observability.(*SlotOperationTrackerManager).StartOperationForConnectionWithMetadata` cumulative 142.10s.
- `gocache/commons/observability.(*connectionContextStore).release` / `reclaimReleased` cumulative 148.39s.

Alloc profile also shows projection/drain allocation pressure, including `SlotOperationTrackerManager.DrainCompletedShard`, `OperationTrackerDrainWorker.projectCompletedOperation`, and `operationTrackerDrainScratch.recordPairs`.

## Started Events Ratio

Prometheus-only run has `pipeline.event.operation_started=0` and no instrumentation subscriber, so G5 is not applicable to that target. The OTEL run was interrupted before completion, so no current measured G5 claim is available. Baseline OTEL data in `zero-allocation-telemetry-benchstats-20260605` has `operation_started=1065160` over `pipeline.evaluations=1500000` (~71.0%), below the >=99% gate.

## Gate Verdict

- Race gates: BLOCKED by `pkg/server` race failure.
- G10 submit-path allocations: PASS at package microbench level (`0 B/op`, `0 allocs/op`), FAIL at mutex-profile condition because submit/context tracker frames remain in pipelined IPC mutex profile.
- G5 started events/ops >=99%: NOT MEASURED in this pass because OTEL run was interrupted; known baseline remains FAIL at ~71%.
- Prometheus IPC allocation/mutex trend: REGRESSED versus provided baseline when normalized by evaluations.
- ADR for P5 no-sink fast-path: NOT WRITTEN. Current evidence is insufficient and gates are blocked/regressed.
