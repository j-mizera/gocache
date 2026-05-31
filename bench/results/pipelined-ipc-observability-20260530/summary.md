# Pipelined IPC observability continuation — 2026-05-30

## Goal

Measure whether batching best-effort IPC observability frames reduces the pipelined IPC overhead left after ADR-0028 / PR #89, while preserving the benchmark attribution modes used by the thesis notes:

- `full`: Prometheus IPC plugin with event forwarding enabled.
- `events-off`: IPC plugin running but command/event emission disabled at the benchmark switch.
- `bridge-off`: event creation remains possible, but the manager does not bridge events to IPC plugins.

## Code under measurement

Branch: `perf/pipelined-ipc-observability`

Primary changes measured here:

- `commons/transport.Conn.SendBatch` writes multiple normal GCPC length-prefixed frames under one transport lock/write call.
- `pkg/plugin/router.PluginConn` uses a per-plugin outbound writer queue and batches bursty frames up to 32 envelopes.
- Fire-and-forget observability frames can use lazy envelope builders so dropped IPC telemetry avoids projection/envelope construction cost.
- A 200 µs maximum delay is applied only while collecting best-effort telemetry batches; blocking request/response frames flush immediately when already present or when they arrive during a telemetry collection window.
- `plugin.ipc` server query output exposes queue/write/batch/drop counters for attribution.
- Follow-up work in the same branch adds pull-based Prometheus command metrics through `server:query:metrics.commands`, so the Prometheus plugin no longer needs one command-completion IPC event per command.

No public GCPC schema changes were introduced; the wire format remains a stream of existing GCPC frames.

## Commands

Package verification and local microbenchmarks:

```bash
go test ./commons/transport
go test ./pkg/plugin/router
go test ./pkg/plugin/manager
go test -race ./commons/transport
go test -race ./pkg/plugin/router
go test -race ./pkg/plugin/manager
go test -race ./api/... ./commons/... ./sdk/... ./plugins/... ./pkg/plugin/...
go test ./...
go test -race ./...
go vet ./...
scripts/check-plugin-isolation.sh
go build ./...
go test -run=^$ -bench=. -benchmem -count=3 ./commons/transport
go test -run=^$ -bench=. -benchmem -count=3 ./pkg/plugin/router

go test ./pkg/metrics ./api/context ./pkg/events ./pkg/pipeline ./pkg/plugin/manager ./pkg/server ./plugins/prometheus
go test -race ./pkg/metrics ./api/context ./pkg/events ./pkg/pipeline ./pkg/plugin/manager ./pkg/server ./plugins/prometheus
go test -run '^$' -bench BenchmarkEvaluateSinks -benchmem ./pkg/pipeline
go test -run '^$' -bench 'Benchmark(Collector|CommandCollector|Bus|FastPath|Evaluate)' -benchmem ./pkg/metrics ./pkg/events ./pkg/pipeline ./plugins/prometheus
```

`staticcheck ./...` was attempted after the full build/vet/isolation checks, but this workstation does not have `staticcheck` installed (`zsh: command not found: staticcheck`).

Docker benchmark captures:

```bash
RESULTS_DIR="$PWD/bench/results/pipelined-ipc-observability-20260530" REBUILD=1 ./bench/redis-benchmark/run-ipc.sh continuation-delay --target gocache-ipc
RESULTS_DIR="$PWD/bench/results/pipelined-ipc-observability-20260530" BENCH_IPC_EVENT_MODE=events-off ./bench/redis-benchmark/run-ipc.sh continuation-delay-events-off --target gocache-ipc
RESULTS_DIR="$PWD/bench/results/pipelined-ipc-observability-20260530" BENCH_IPC_EVENT_MODE=bridge-off ./bench/redis-benchmark/run-ipc.sh continuation-delay-bridge-off --target gocache-ipc
RESULTS_DIR="$PWD/bench/results/pipelined-ipc-observability-20260530" REBUILD=1 ./bench/redis-benchmark/run-ipc.sh continuation-pull-metrics --target gocache-ipc
```

The earlier same-directory captures provide the current-core, Valkey, first no-delay IPC, events-off, and bridge-off comparison points. The PR #89 anchor is `../heavy-event-hotpath-20260530/implementation-gocache-ipc*.csv`.

Docker emitted a non-fatal local tooling warning before falling back to the legacy builder:

```text
failed to fetch metadata: fork/exec /home/witherxse/.docker/cli-plugins/docker-buildx: no such file or directory
```

## Local package benchmark signal

`commons/transport` continued to show the intended microbenchmark shape after delayed batching:

| Benchmark | ns/op range | B/op | allocs/op |
|---|---:|---:|---:|
| `send_recv_roundtrip` | 2435–2458 | 557 | 13 |
| `send_recv_batch_32` | 2187–2270 | 577 | 12 |

`pkg/plugin/router` enqueue stayed zero-alloc in the focused benchmark:

| Benchmark | ns/op range | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkPluginConnSendFireAndForget` | 140.1–141.9 | 35–36 | 0 |

The follow-up pull-metrics package signal shows the new metrics-only pipeline branch is close to the no-sink path and materially cheaper than command-completion event delivery:

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkEvaluateSinks/no_sink` | 340.9 | 336 | 9 |
| `BenchmarkEvaluateSinks/command_metrics_only` | 357.9 | 336 | 9 |
| `BenchmarkEvaluateSinks/event_completed_only` | 1123 | 1176 | 23 |
| `BenchmarkCollectorReplaceFromQuery` | 727.8 | 688 | 18 |

## Aggregate Docker deltas

All deltas below are geometric means across the 15 redis-benchmark commands emitted by the harness. Positive RPS is better; negative p99 latency is better.

| Comparison | Standard RPS | Standard p99 | Pipelined RPS | Pipelined p99 |
|---|---:|---:|---:|---:|
| delayed IPC full vs PR #89 IPC anchor | +3.05% | -46.48% | +13.89% | -6.07% |
| delayed IPC full vs Valkey reference | +2.11% | +5.88% | -51.35% | +444.23% |
| delayed IPC full vs current GoCache core capture | +3.53% | -15.40% | -42.19% | +109.57% |
| delayed IPC full vs delayed events-off | -6.96% | +48.46% | -49.10% | +162.59% |
| delayed IPC full vs delayed bridge-off | +4.00% | +28.99% | -24.53% | +76.09% |
| delayed IPC full vs first no-delay IPC capture | +18.35% | -67.74% | +20.66% | -10.48% |
| pull-metrics IPC vs delayed IPC full | -0.89% | +13.68% | +55.23% | -44.58% |
| pull-metrics IPC vs delayed bridge-off | +3.08% | +46.64% | +17.16% | -2.42% |
| pull-metrics IPC vs delayed events-off | -7.78% | +68.78% | -20.98% | +45.51% |
| pull-metrics IPC vs current GoCache core capture | +2.62% | -3.83% | -10.26% | +16.13% |
| pull-metrics IPC vs Valkey reference | +1.21% | +20.37% | -24.47% | +201.59% |

## Memory samples

| Capture | Final RSS bytes | Delta RSS bytes |
|---|---:|---:|
| delayed IPC full | 185,388,236 | 166,157,353 |
| delayed events-off | 160,117,555 | 116,182,221 |
| delayed bridge-off | 196,398,284 | 152,483,922 |
| pull-metrics IPC | 170,183,884 | 128,335,216 |
| Valkey reference | 38,157,680 | 19,933,430 |
| current GoCache core capture | 148,268,646 | 116,989,624 |

## Interpretation

Delayed telemetry batching is a measurable improvement over the first no-delay continuation capture and over the PR #89 IPC anchor, especially for pipelined RPS. Pull-based Prometheus metrics are the stronger pipelined improvement: versus delayed IPC full, pipelined geometric-mean RPS improves by +55.23% and p99 improves by -44.58% while preserving the existing GCPC frame schema.

The pull-metrics run is close to the current core capture but does not fully match it in pipelined mode (`-10.26%` RPS, `+16.13%` p99), and it remains behind Valkey in pipelined mode (`-24.47%` RPS, `+201.59%` p99). It also remains slower than the synthetic delayed events-off attribution run, which confirms there is still non-event IPC/plugin overhead to investigate. The follow-up pull-metrics change addresses the Prometheus portion directly by recording compact command aggregates in-process and exposing them through `server:query:metrics.commands`; exact event delivery remains available for plugins that require full records.

## Caveats

- These are single-run Docker harness captures, not multi-run statistical suites.
- The benchmark host emitted a local `docker-buildx` plugin warning and used the legacy Docker builder.
- Memory numbers are coarse container RSS samples from the harness and should not be used as the main comparison axis for this PR.
- The branch intentionally preserves the existing GCPC wire format and does not attempt a public protocol-level batch message.
