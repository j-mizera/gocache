---
title: Comparison Methodology
description: Baseline provenance and Redis-compatible benchmark comparison rules for thesis evidence
status: living
last_updated: 2026-07-08
related:
  - Performance
  - Benchmark Controls
---

# Comparison Methodology

This document defines the evidence contract for GoCache benchmark provenance and Redis/Valkey-compatible comparisons. It is the final methodology layer for the benchmark suite: benchmark numbers are thesis-grade only when they can be traced back to a controlled host, a reproducible command, a specific GoCache revision, and a comparison harness that measures the same workload on both systems.

## 1. Baseline Provenance

Every evidence-grade benchmark capture must include a baseline provenance record. The provenance record is not a performance result by itself; it is the metadata that makes a performance result reproducible and comparable.

### Measurement host

Record the host configuration beside every benchmark result:

- CPU model.
- Physical core count and logical CPU count.
- RAM capacity.
- Storage type used by the repository, benchmark output directory, and any persistence files involved in the run.
- Kernel version.
- Platform: native Linux, WSL2, containerized Linux, or another explicitly named environment.

Native Linux, WSL2, and containerized results are separate evidence classes. Do not compare them as equivalent unless the thesis claim is explicitly about environment sensitivity.

### Isolation level

Record which controls from [Benchmark Controls](benchmark-controls.md) were applied:

- CPU governor set to `performance`, or the omission documented.
- Turbo Boost disabled, or the omission documented with the CPU vendor and control path.
- NUMA pinning applied for multi-NUMA hosts, or marked not applicable for single-node hosts.
- Competing load removed from the host, or listed as a caveat.

The isolation record must say what was actually applied, not only what was intended. If a control was unavailable, the result can still be useful, but the evidence must not present it as equivalent to a fully isolated run.

### Reproducibility mode

The default reproducibility mode is **median-of-10** using `benchstat` over ten independent benchmark runs. For Go benchmarks, use the FR-006 wrapper path documented by `bench/profiles/run-benchstat.sh`, which runs benchmarks with `-count=10` and analyzes the result with `benchstat`.

When reporting percentages, state whether the percentage is:

- median-of-runs, preferred for thesis evidence;
- best-of-runs, allowed only for exploratory ceilings and labeled as such;
- another statistic, with the calculation described next to the result.

Best-of-runs must not be mixed with median-of-runs in the same comparison table.

### Go toolchain

Record the exact Go version for each capture. Go compiler escape analysis, inlining, runtime scheduling, and allocation accounting can shift `allocs/op`, `B/op`, and latency distributions between releases. Allocation deltas are comparable only when both sides of the comparison use the same Go toolchain, or when the Go toolchain change is explicitly treated as a methodology change.

### Benchmark date and commit SHA

Use `CaptureBaselineProvenance` from `pkg/plugin/benchsuite/provenance.go` for benchmark-suite captures. The current provenance payload records:

- `commit_sha` from `git rev-parse HEAD`;
- `go_version` from the running Go toolchain;
- UTC `date` in RFC3339 format;
- `goarch`;
- `num_cpu`;
- a compact `hardware` string.

`LockBaseline` writes this payload under `bench/results/baseline-*.json`. Treat that JSON file as the provenance anchor for Go benchmark evidence, then supplement it with the host fields above when the compact payload does not include enough detail for a thesis table.

## 2. Redis/Valkey Comparison Methodology

**THE RULE:** Any Redis/Valkey performance comparison in thesis evidence MUST use `redis-benchmark` or `valkey-benchmark` with identical flags against both systems.

Redis-compatible comparisons are client-visible comparisons. They measure how a Redis-compatible client observes Valkey and GoCache under the same workload. They do not substitute for Go microbenchmarks, and Go microbenchmarks do not substitute for Redis-compatible client benchmarks.

### Required identical flags

The following flags must be identical for both systems in any side-by-side comparison:

- `-c <clients>`: number of concurrent connections, for example `-c 50`.
- `-n <requests>`: total number of requests, for example `-n 100000`.
- `-d <data-size>`: payload size in bytes, for example `-d 64`.
- `-P <pipelining>`: number of pipelined requests, for example `-P 1` for no pipelining or `-P 16` for pipelined traffic.
- `-r <keyspacelen>`: random key space length, for example `-r 1000000` to prevent hot-key caching from dominating the result.

If a benchmark script exposes these values through environment variables, record the resolved values in the result summary. The comparison is invalid if one side uses a different client count, request count, payload size, pipeline depth, or keyspace length.

### Existing comparison infrastructure

Use the repository harnesses in `bench/redis-benchmark/` before writing an ad hoc Redis-compatible comparison:

- `task bench:redis` runs the `valkey-benchmark` harness against GoCache core or Valkey, depending on the selected target.
- `task bench:redis:ipc` runs the `valkey-benchmark` harness against the GoCache IPC plugin path.
- `task bench:redis:matrix` runs the full Valkey/Core/IPC matrix.
- `task bench:redis:pubsub` runs the Pub/Sub fan-out benchmark.

The default matrix is documented in `bench/redis-benchmark/README.md`. It records standard and pipelined CSV output under `bench/results/<branch>/`, along with memory and configuration artifacts where relevant.

### Forbidden comparison patterns

The following comparison patterns are invalid for thesis evidence:

1. **NEVER** compare a GoCache microbenchmark (`ns/op` from `go test -bench`) to a published Redis benchmark number from a different harness. These are apples-to-oranges: different methodology, workload, and concurrency model.
2. **NEVER** compare GoCache's in-process event bus latency, such as `BenchmarkBusEmit`, to Redis `PUBLISH` latency. GoCache's bus is in-process; Redis `PUBLISH` crosses a network socket.
3. **NEVER** compare GoCache's PluginConn RTT, such as `BenchmarkPluginCommandRTT`, to `redis-benchmark` GET/SET latency. PluginConn measures IPC to a plugin subprocess, not client-facing command latency.

### Valid comparison patterns

The following patterns are valid when the commands, host, date, and commit SHA are recorded:

1. Run `task bench:redis:matrix` to execute `valkey-benchmark` with identical flags against Valkey as the Redis-compatible baseline and against GoCache. Report both numbers side-by-side.
2. Compare the **scaling shape**: how throughput changes with concurrency, client count, pipeline depth, or fan-out. GoCache's goroutine-based concurrency model differs from Redis's single-threaded event loop, so absolute numbers measure different implementation shapes.
3. Cite benchmark numbers only with the exact command used, host configuration, benchmark date, and GoCache commit SHA.

When the thesis needs an absolute headline ratio, the ratio must come from the same `redis-benchmark` or `valkey-benchmark` invocation family on the same host class. Do not derive a ratio by mixing a Go benchmark, a Docker benchmark, and a published external number.

## 3. Thesis Evidence Requirements

Any performance number cited in thesis evidence must satisfy the following checklist:

1. It is backed by `benchstat` with `-count=10` for Go benchmark evidence, satisfying FR-006 statistical rigor.
2. It cites the relevant tiered reproducibility threshold and confirms whether the result met that threshold, satisfying FR-007.
3. It includes provenance metadata: host controls, Go toolchain, date, and commit SHA, satisfying FR-010.
4. It uses the Redis/Valkey comparison methodology in this document whenever the number is compared to Redis or Valkey.
5. It marks the benchmark suite version as `v1-pre-aof` and states the scope being measured, satisfying FR-012.

Recommended evidence footer for thesis tables:

```text
Suite: v1-pre-aof
Scope: <core | IPC plugin path | Pub/Sub fan-out | Go microbenchmark package>
Command: <exact command or Task invocation>
Host: <CPU, cores, RAM, storage, kernel, platform>
Controls: <governor, Turbo Boost, NUMA, competing load>
Statistic: <median-of-10 benchstat | best-of-runs exploratory | other named statistic>
Date: <UTC date>
Commit: <GoCache commit SHA>
Comparison rule: <none | identical valkey-benchmark flags>
```

If any footer field is missing, the thesis text must state the omission and narrow the claim accordingly.
