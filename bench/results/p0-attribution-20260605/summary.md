# P0 Attribution Prep Results - 2026-06-05

Branch: `zero-allocation-telemetry-subplan-b`

## Harness Changes Covered

- `bench/redis-benchmark/run-ipc.sh` supports `gocache-ipc`, `gocache-ipc-otel`, and `gocache-core-probe` targets.
- `BENCH_PPROF=1` captures heap and mutex profiles around the pipelined repeat window for `gocache-ipc` and `gocache-ipc-otel`, then writes top-20 allocation object/space and top-10 mutex reports.
- `BENCH_REPEAT` defaults to `3`; canonical CSVs report per-command medians in the original numeric columns and matching `*_iqr` columns. Raw repeat CSVs are retained under `*-repeats/`.
- `gocache-core-probe` builds/runs only the benchmark `benchprobe` IPC plugin; no Prometheus or instrumentation plugin is configured.

## Captures

All captures used `BENCH_N=20000` and `BENCH_REPEAT=3`, so `claim_grade=claim-ready` in repeat metadata.

| Target | Profiled | Key artifacts |
|--------|----------|---------------|
| `gocache-ipc` | yes | `p0-attribution-gocache-ipc.csv`, `p0-attribution-gocache-ipc-pipelined.csv`, `p0-attribution-gocache-ipc-profiles/` |
| `gocache-ipc-otel` | yes | `p0-attribution-gocache-ipc-otel.csv`, `p0-attribution-gocache-ipc-otel-pipelined.csv`, `p0-attribution-gocache-ipc-otel-profiles/` |
| `gocache-core-probe` | no | `p0-attribution-gocache-core-probe.csv`, `p0-attribution-gocache-core-probe-pipelined.csv`, `p0-attribution-gocache-core-probe-benchstats-*.json` |

## Attribution Pointers

- Prometheus-only IPC alloc profile top cumulative site: `gocache/pkg/server.(*Server).handleConnection` at 20,428,314 alloc objects cumulative in the pipelined diff window.
- Prometheus-only IPC mutex profile top cumulative non-runtime path: `gocache/pkg/server.(*Server).handleConnection` / shard unlock path, with `sync.(*Mutex).Unlock` accounting for the sampled delay root.
- IPC+OTEL alloc profile top cumulative site: `gocache/pkg/plugin/router.(*PluginConn).writeLoop`, followed by `writeOutboundBatch`, drain-worker projection, and protobuf marshal paths.
- IPC+OTEL mutex profile top cumulative non-runtime path: `gocache/pkg/server.(*Server).handleConnection` / shard unlock path.

## Verification Notes

- `RESULTS_DIR="$PWD/bench/results/p0-attribution-20260605" BENCH_N=20000 BENCH_REPEAT=3 REBUILD=0 ./bench/redis-benchmark/run-ipc.sh p0-attribution --target gocache-core-probe` completed using cached `gocache-bench:local-gocache-core-probe`.
- A rebuild of the core-probe image from the current worktree failed because `commons/observability/slot_tracker.go` has unrelated compile errors in in-progress telemetry logic. That file is outside this P0 harness task and was not modified here.
- `RESULTS_DIR="$PWD/bench/results/p0-attribution-20260605" ./bench/redis-benchmark/compare.sh p0-attribution-gocache-core-probe p0-attribution-gocache-ipc` successfully consumed the median/IQR CSV shape.
