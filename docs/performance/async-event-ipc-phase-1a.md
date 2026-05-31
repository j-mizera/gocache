---
title: Async Event IPC Phase 1A
description: Baseline measurement hooks and rollbackable yamux spike plan for async event IPC backpressure work
status: active
date: 2026-05-29
related:
  - ../adr/0026-event-traffic-classes-and-backpressure.md
  - modular-overhead-optimization-plan.md
---

# Async Event IPC Phase 1A

Phase 1A follows ADR-0026's "cheapest credible reversible learning first" rule. It combines mandatory baseline measurement hooks on the current GCPC FIFO path with a narrow yamux spike plan. It does **not** approve a full GCPC migration, protobuf schema change, or permanent transport replacement.

## Baseline hooks

The current implementation must remain the control path. The first measurement surface is `server_query` topic `plugin.ipc`, exposed through the existing GCPC query mechanism. Plugins need `server:query:plugin.ipc`, wildcard `server:query`, or `admin` scope to read it; counters are per plugin connection and reset when that connection is recreated after reconnect or restart.

Per plugin connection, the query reports:

- `queue_capacity`
- `queue_depth`
- `send_attempts`
- `send_accepted`
- `send_queue_full`
- `send_plugin_down`
- `send_context_cancelled`
- `blocking_send_attempts`
- `blocking_send_latency_total_ns`
- `blocking_send_latency_max_ns`
- `fire_and_forget_attempts`
- `fire_and_forget_accepted`
- `fire_and_forget_drops`
- `enqueue_latency_total_ns`
- `enqueue_latency_max_ns`
- `write_attempts`
- `write_errors`
- `write_batches`
- `write_batch_envelopes`
- `write_batch_max_size`
- `write_latency_total_ns`
- `write_latency_max_ns`
- `queue_lag_total_ns`
- `queue_lag_max_ns`

These hooks intentionally measure the per-plugin FIFO queue and framed protobuf write path. The original Phase 1A hook work did not introduce traffic classes, priority scheduling, batching, or stream IDs; the later 2026-05-30 continuation added normal-frame write batching while preserving this query surface for attribution.

## Yamux spike boundary

The yamux spike is allowed only as a reversible experiment with the same measurements as the baseline. The spike should test whether logical streams over one local connection can isolate critical/control traffic from best-effort runtime events with less custom scheduling work than a new GCPC queue topology.

### Must prove

- A server-side session and plugin-side session can run over the existing Unix-domain socket shape or an equivalently local test connection.
- At least two logical streams can be opened and accepted: one critical/control lane and one best-effort event lane.
- Existing GCPC envelopes can still be framed per logical stream without changing `api/gcpc/v1/gcpc.proto`.
- A stalled event stream does not inflate critical/control enqueue or write latency beyond the current FIFO baseline.
- Shutdown, reconnect, and plugin process exit do not leak goroutines or file descriptors.

### Must not do

- Do not change GCPC message schema.
- Do not migrate all plugin traffic to yamux.
- Do not remove the current `commons/transport` framed protobuf path.
- Do not make Prometheus-specific core behavior.
- Do not accept the library based on ergonomics alone; compare against the baseline hook data.

## Rollback plan

- Keep current GCPC transport as the default and control path.
- Hide yamux behind one adapter or build-tagged experiment path.
- Keep dependency addition isolated to the spike commit/branch until benchmark evidence justifies it.
- If critical latency, RSS, goroutine count, or shutdown behavior does not improve enough, remove the adapter and dependency without touching GCPC schema or plugin semantics.

## Benchmark gate

Start with the existing baseline captures under `bench/results/perf-async-event-ipc/`, then rerun the dockerized benchmark harness with a new label only after hooks compile and tests pass.

Minimum comparisons:

```bash
./bench/redis-benchmark/compare.sh async-event-ipc-baseline-20260529-gocache async-event-ipc-baseline-20260529-gocache-ipc
./bench/redis-benchmark/compare.sh async-event-ipc-baseline-20260529-gocache async-event-ipc-baseline-20260529-valkey
```

Implementation or spike candidates should use the same command mix and report the `plugin.ipc` query snapshot alongside throughput, p99, RSS, and any profile captures.

## 2026-05-29 hook benchmark evidence

Hook-enabled captures are stored under `bench/results/perf-async-event-ipc/` with label `async-event-ipc-phase1a-hooks-20260529`. The run used the same dockerized matrix parameters as the baseline capture: `n=100000`, `clients=50`, `keyspace=100000`, `pipeline=10`, target CPUs `0-3`, client CPUs `4-7`, and memory limit `2g`.

Compared with `async-event-ipc-baseline-20260529`, hook-enabled IPC throughput stayed within one-run variance:

| Comparison | Standard median RPS delta | Pipelined median RPS delta | Notes |
|---|---:|---:|---|
| `gocache-ipc` hooks vs previous `gocache-ipc` baseline | -1.00% | -0.49% | Min/max spread was -4.47%/+12.64% standard and -10.78%/+6.68% pipelined. |
| `gocache` hooks build vs previous `gocache` baseline | +4.77% | +4.02% | No IPC plugin active; positive deltas are treated as run variance, not an optimization claim. |
| Temporary `ipcprobe` listener vs hook-enabled `gocache-ipc` | -0.89% | -0.65% | Probe snapshots were taken outside the timed benchmark loops to avoid query-path perturbation. |

The hook-enabled matrix still shows the modular IPC bottleneck that ADR-0026 is targeting: median `gocache-ipc / gocache` RPS ratio is about `0.96x` for standard workload but only about `0.31x` for pipelined workload. Median `gocache / valkey` ratio in the same hook-enabled run is about `0.96x` standard and `0.87x` pipelined, so the large pipelined regression is isolated to the IPC plugin path rather than the no-plugin control path.

RSS captures from the same run:

| Target | Baseline RSS | Post-standard RSS | Final RSS | Delta RSS |
|---|---:|---:|---:|---:|
| `gocache` | 35,903,242 | 143,025,766 | 187,065,958 | 151,162,716 |
| `gocache-ipc` | 43,631,247 | 191,050,547 | 225,024,409 | 181,393,162 |
| `gocache-ipc-probe` | 49,094,328 | 213,490,073 | 233,413,017 | 184,318,689 |
| `valkey` | 8,941,207 | 24,452,792 | 27,409,776 | 18,468,569 |

The temporary `ipcprobe` run captured `plugin.ipc` snapshots before workload, after standard workload, and after pipelined workload:

| Snapshot interval | Send attempts | Accepted | Queue-full drops | Drop rate | Avg enqueue latency | Avg queue lag | Avg write latency | Write errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Standard increment | 1,500,000 | 1,500,000 | 0 | 0.00% | 981 ns | 145,037 ns | 4,223 ns | 0 |
| Pipelined increment | 1,500,000 | 1,141,215 | 358,785 | 23.92% | 1,277 ns | 2,655,681 ns | 4,995 ns | 0 |
| Cumulative workload | 3,000,000 | 2,641,215 | 358,785 | 11.96% | 1,129 ns | 1,229,835 ns | 4,557 ns | 0 |

This establishes that the measurement hooks are usable for Phase 1B: standard load drains the current FIFO with no drops, while pipelined load saturates the best-effort event queue and records queue-full drops without write errors. The data is a single-run gate for choosing the next reversible spike, not statistical proof of final overhead.

## 2026-05-29 Phase 1B attribution measurements

Phase 1B added two measurement-only IPC benchmark modes to `bench/redis-benchmark/run-ipc.sh`. Default behavior remains `BENCH_IPC_EVENT_MODE=full`.

| Mode | Mechanism | What it isolates |
|---|---|---|
| `events-off` | Removes the Prometheus plugin's `events` scope from the generated benchmark config while keeping health/plugin server-query scopes. | Plugin process/readiness overhead remains, but the event bus has no subscriber, so the pipeline should take the `hasAnySink()` fast path and skip command event construction. |
| `bridge-off` | Keeps the event subscription active and sets `GOCACHE_BENCH_EVENT_BRIDGE_MODE=bridge-off`, making the manager event bridge return before clone/filter/envelope/FIFO enqueue. | Event construction and bus dispatch remain, but IPC event enqueue/write/plugin-drain work is removed. |

Commands:

```bash
REBUILD=1 BENCH_IPC_EVENT_MODE=events-off ./bench/redis-benchmark/run-ipc.sh async-event-ipc-phase1b-events-off-20260529 --target gocache-ipc
./bench/redis-benchmark/compare.sh async-event-ipc-phase1a-hooks-20260529-gocache-ipc async-event-ipc-phase1b-events-off-20260529-gocache-ipc

BENCH_IPC_EVENT_MODE=bridge-off ./bench/redis-benchmark/run-ipc.sh async-event-ipc-phase1b-bridge-off-20260529 --target gocache-ipc
./bench/redis-benchmark/compare.sh async-event-ipc-phase1a-hooks-20260529-gocache-ipc async-event-ipc-phase1b-bridge-off-20260529-gocache-ipc
```

Median RPS deltas against the Phase 1A full IPC hook run:

| Variant | Standard median RPS delta | Pipelined median RPS delta | Pipelined ratio vs Phase 1A no-IPC `gocache` control |
|---|---:|---:|---:|
| `events-off` | -1.4% | +215.6% | 0.983x |
| `bridge-off` | +4.3% | +38.4% | 0.454x |
| Phase 1A full IPC | — | — | 0.307x |

The `events-off` variant recovers almost all pipelined throughput relative to the no-IPC GoCache control. The `bridge-off` variant improves materially over full IPC but remains far below the no-IPC control. That split indicates most pipelined throughput loss is paid before IPC enqueue/write attempts in the event-enabled hot path: operation tracking/enrichment, context snapshots, event construction, bus dispatch, and subscriber callback invocation. The bridge/FIFO/write path still matters for drops, lag, critical-traffic isolation, and the remaining `0.454x -> 0.307x` gap, but this run says raw pipelined command throughput should prioritize producer-side interest masks or earlier event gating before yamux/FIFO scheduling work.

RSS captures:

| Target/mode | Baseline RSS | Post-standard RSS | Final RSS | Delta RSS |
|---|---:|---:|---:|---:|
| Phase 1A `full` | 43,631,247 | 191,050,547 | 225,024,409 | 181,393,162 |
| Phase 1B `events-off` | 43,568,332 | 152,253,235 | 188,638,822 | 145,070,490 |
| Phase 1B `bridge-off` | 43,253,760 | 190,001,971 | 228,904,140 | 185,650,380 |

As before, these are single-run attribution measurements, not final statistical proof. They are strong enough to order the next optimization work: producer-side masks/gates first, then batching/queue/transport isolation once event production cost is reduced.

## 2026-05-30 normal-frame batching continuation

The follow-up branch `perf/pipelined-ipc-observability` kept the GCPC schema unchanged and batched ordinary length-prefixed frames in the per-plugin writer. The raw captures and summary live under `bench/results/pipelined-ipc-observability-20260530/`.

The continuation added these `plugin.ipc` attribution counters:

- `write_batches`
- `write_batch_envelopes`
- `write_batch_max_size`

Compared with the PR #89 IPC anchor, delayed batching improved IPC Prometheus pipelined geometric-mean RPS by `+13.89%` and p99 by `-6.07%`. It still left the pipelined IPC configuration materially behind Valkey (`-51.35%` RPS) and the current no-IPC GoCache core capture (`-42.19%` RPS), so the next performance lever remains reduced event volume and cheaper projection for metrics-only consumers rather than a public protocol-level batch message.
