# GoCache variants vs Valkey and previous benchstats

Date: 2026-06-05  
Current result dirs: `bench/results/zero-allocation-telemetry-core-20260605/`, `bench/results/operationtracker-telemetry-post-otel/`  
Previous comparison dirs: `bench/results/bench-discovered-fixes-baseline/`, `bench/results/runtime-otlp-benchstats-20260531/`

Note: the existing IPC harness target named `gocache-ipc-otel` is **Core + Prometheus + instrumentation**, because Prometheus remains present for readiness/metrics and instrumentation exports OTLP through the local collector. A clean instrumentation-only target would need a harness change.

## Table 1 — Standard mode: GoCache variants vs same-day Valkey

| Variant | Avg vs Valkey | Median vs Valkey | SET | GET | INCR | LRANGE_100 | MSET |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Core (`gocache`) | 0.98x (-2.5%) | 0.97x (-2.7%) | 0.95x (-5.5%) | 0.98x (-1.8%) | 1.01x (+1.2%) | 0.93x (-7.3%) | 0.97x (-3.2%) |
| Core + Prometheus IPC (`gocache-ipc`) | 0.98x (-2.1%) | 0.99x (-1.5%) | 0.92x (-7.7%) | 0.97x (-3.1%) | 1.02x (+2.3%) | 0.96x (-3.9%) | 0.98x (-1.9%) |
| Core + Prometheus + instrumentation IPC (`gocache-ipc-otel`) | 0.84x (-15.7%) | 0.84x (-15.7%) | 0.82x (-17.7%) | 0.81x (-18.6%) | 0.84x (-15.8%) | 0.87x (-13.3%) | 0.78x (-21.8%) |

## Table 2 — Pipelined mode: GoCache variants vs same-day Valkey

| Variant | Avg vs Valkey | Median vs Valkey | SET | GET | INCR | LRANGE_100 | MSET |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Core (`gocache`) | 0.72x (-27.5%) | 0.81x (-18.7%) | 0.83x (-17.0%) | 0.98x (-2.5%) | 0.89x (-10.9%) | 0.81x (-18.7%) | 0.81x (-18.7%) |
| Core + Prometheus IPC (`gocache-ipc`) | 0.69x (-30.6%) | 0.78x (-21.7%) | 0.82x (-18.1%) | 0.90x (-10.0%) | 0.77x (-23.3%) | 0.80x (-20.2%) | 0.82x (-18.2%) |
| Core + Prometheus + instrumentation IPC (`gocache-ipc-otel`) | 0.39x (-60.8%) | 0.33x (-67.1%) | 0.34x (-66.5%) | 0.55x (-44.8%) | 0.32x (-67.7%) | 0.58x (-42.0%) | 0.48x (-52.0%) |

## Table 3 — Current benchstats/memory vs previous bench

| Variant | Mode | Avg RPS vs prev | Pipeline path prev -> current | Events emitted prev -> current | Context snapshots prev -> current | Projections prev -> current | Alloc bytes prev -> current | Alloc objects prev -> current | Mutex wait prev -> current | RSS prev -> current |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Core (`gocache`) | standard | -20.6% | N/A (core harness has no benchprobe) | N/A | N/A | N/A | N/A | N/A | N/A | 140.9 MiB -> 173.8 MiB (+23.3%) |
| Core (`gocache`) | pipelined | -39.2% | N/A (core harness has no benchprobe) | N/A | N/A | N/A | N/A | N/A | N/A | 177.6 MiB -> 202.0 MiB (+13.7%) |
| Core + Prometheus IPC (`gocache-ipc`) | standard | -3.7% | fast 0 / metrics 1.50M / full 0 -> fast 0 / metrics 0 / full 1.50M | 0 -> 0 (N/A) | 0 -> 0 (N/A) | 0 -> 0 (N/A) | 2.2 GiB -> 3.2 GiB (+43.4%) | 23.76M -> 52.31M (+120.1%) | 1.326631s -> 2.340445s (+76.4%) | 160.2 MiB -> 193.4 MiB (+20.7%) |
| Core + Prometheus IPC (`gocache-ipc`) | pipelined | -24.0% | fast 0 / metrics 1.50M / full 0 -> fast 0 / metrics 0 / full 1.50M | 0 -> 0 (N/A) | 0 -> 0 (N/A) | 0 -> 0 (N/A) | 4.5 GiB -> 5.9 GiB (+31.2%) | 48.61M -> 93.66M (+92.7%) | 72.982904s -> 102.475394s (+40.4%) | 185.9 MiB -> 218.4 MiB (+17.5%) |
| Core + Prometheus + instrumentation IPC (`gocache-ipc-otel`) | standard | -4.7% | fast 0 / metrics 0 / full 1.50M -> fast 0 / metrics 0 / full 1.50M | 3.00M -> 3.00M (+0.1%) | 3.00M -> 0 (-100.0%) | 2.93M -> 2.50M (-14.8%) | 9.0 GiB -> 10.3 GiB (+14.6%) | 150.34M -> 189.44M (+26.0%) | 18.601042s -> 13.719978s (-26.2%) | 221.5 MiB -> 251.9 MiB (+13.7%) |
| Core + Prometheus + instrumentation IPC (`gocache-ipc-otel`) | pipelined | +27.3% | fast 0 / metrics 0 / full 1.50M -> fast 0 / metrics 0 / full 1.50M | 3.00M -> 2.13M (-28.9%) | 3.00M -> 0 (-100.0%) | 1.26M -> 880.30K (-30.0%) | 15.7 GiB -> 16.7 GiB (+6.6%) | 252.29M -> 299.38M (+18.7%) | 246.547554s -> 203.762801s (-17.4%) | 222.8 MiB -> 296.5 MiB (+33.1%) |

## Reading the tables

- Same-day standard mode is roughly at Valkey parity for core and Prometheus-only IPC; instrumentation is lower but still within ~0.84x average of Valkey.
- Same-day pipelined mode is where the cliff remains: core averages 0.72x Valkey, Prometheus-only IPC 0.69x, and Prometheus+instrumentation 0.39x.
- Prometheus-only changed from the previous metrics-only path to full path under the new always-operation telemetry model. That increases RSS/allocation counters, but same-day throughput remains close to core.
- Instrumentation improved vs previous pipelined `gocache-ipc-otel` throughput, but it still emits ~2.13M events and builds ~880K projections in the pipelined attribution window, which explains the large same-day gap against Prometheus-only IPC.
- Core-only allocation/lock benchstats are N/A because `benchprobe` is an IPC plugin and the core harness intentionally builds with `PLUGINS=""`.
