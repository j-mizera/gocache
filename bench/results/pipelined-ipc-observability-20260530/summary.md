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
```

`staticcheck ./...` was attempted after the full build/vet/isolation checks, but this workstation does not have `staticcheck` installed (`zsh: command not found: staticcheck`).

Docker benchmark captures:

```bash
RESULTS_DIR="$PWD/bench/results/pipelined-ipc-observability-20260530" REBUILD=1 ./bench/redis-benchmark/run-ipc.sh continuation-delay --target gocache-ipc
RESULTS_DIR="$PWD/bench/results/pipelined-ipc-observability-20260530" BENCH_IPC_EVENT_MODE=events-off ./bench/redis-benchmark/run-ipc.sh continuation-delay-events-off --target gocache-ipc
RESULTS_DIR="$PWD/bench/results/pipelined-ipc-observability-20260530" BENCH_IPC_EVENT_MODE=bridge-off ./bench/redis-benchmark/run-ipc.sh continuation-delay-bridge-off --target gocache-ipc
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

## Memory samples

| Capture | Final RSS bytes | Delta RSS bytes |
|---|---:|---:|
| delayed IPC full | 185,388,236 | 166,157,353 |
| delayed events-off | 160,117,555 | 116,182,221 |
| delayed bridge-off | 196,398,284 | 152,483,922 |
| Valkey reference | 38,157,680 | 19,933,430 |
| current GoCache core capture | 148,268,646 | 116,989,624 |

## Interpretation

Delayed telemetry batching is a measurable improvement over the first no-delay continuation capture and over the PR #89 IPC anchor, especially for pipelined RPS. It does not close the thesis-visible pipelined IPC gap: with Prometheus IPC observability enabled, GoCache remains materially behind both the current core capture and Valkey in pipelined mode.

The attribution captures still show that event forwarding/projection dominates the pipelined IPC overhead. `events-off` remains much faster than `full`, and `bridge-off` also beats `full` in pipelined mode, so further gains likely need fewer per-command observability events, coarser aggregation, or a redesigned event bridge rather than only lower-level frame batching.

## Caveats

- These are single-run Docker harness captures, not multi-run statistical suites.
- The benchmark host emitted a local `docker-buildx` plugin warning and used the legacy Docker builder.
- Memory numbers are coarse container RSS samples from the harness and should not be used as the main comparison axis for this PR.
- The branch intentionally preserves the existing GCPC wire format and does not attempt a public protocol-level batch message.
