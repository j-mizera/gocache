---
title: Benchmark Controls
description: Hardware and toolchain controls for evidence-grade GoCache benchmark runs
status: living
last_updated: 2026-07-08
related:
  - Performance
  - ADR-0022-modular-performance-budget
---

# Benchmark Controls

Evidence-grade benchmark runs must record enough host context for thesis readers to understand what was controlled, what remained variable, and which runs are comparable. These controls apply to final benchmark captures and threshold validation runs. Exploratory local runs may skip them, but the resulting numbers should not be presented as evidence-grade.

The Valkey benchmark methodology is the external compatibility reference for this checklist: <https://valkey.io/topics/benchmark/>.

## CPU Governor

Use the Linux `performance` CPU frequency governor before recording evidence-grade results:

```sh
sudo cpupower frequency-set -g performance
```

Record whether this command succeeded. If `cpupower` is unavailable or root access is not available, document the omission next to the benchmark result.

## Turbo Boost

Disable CPU boost so repeated runs are less affected by short-lived frequency spikes.

On AMD systems:

```sh
echo 0 > /sys/devices/system/cpu/cpufreq/boost
```

On Intel systems using `intel_pstate`:

```sh
echo 1 > /sys/devices/system/cpu/intel_pstate/no_turbo
```

Record the CPU vendor and the exact boost control used. Do not compare boosted and non-boosted runs as if they were taken under the same hardware controls.

## NUMA Pinning

Pin evidence-grade benchmark processes to one NUMA node when running on multi-socket or multi-NUMA hosts:

```sh
numactl --cpunodebind=0 --membind=0 go test -bench=...
```

Record the NUMA binding in the result summary. Single-node developer machines can mark this control as not applicable, but should still record the host topology if available.

## No Competing Load

Run benchmarks on an otherwise quiet host. Stop browsers, IDEs, Docker containers, background build jobs, local databases, and other CPU- or IO-heavy processes before recording final captures.

If competing load cannot be eliminated, document it as a caveat and treat the run as weaker evidence.

## Go Toolchain Version

The Go version is captured by `CaptureBaselineProvenance` and by the evidence-grade run wrapper. Cross-version allocation comparisons are invalid unless the comparison explicitly controls for Go runtime and compiler changes.

When comparing two commits, use the same Go toolchain for both sides. If the toolchain changes, report it as a methodology change rather than as an application-code delta.

## Kernel Version and Platform

Record the kernel version and platform for every evidence-grade capture. Native Linux results are not directly comparable to WSL2 results because syscall, scheduler, filesystem, and virtualization behavior can differ.

For thesis evidence, compare native Linux to native Linux, WSL2 to WSL2, and containerized runs only to runs with matching container assumptions.

## Evidence-Grade Run Checklist

- [ ] Go version recorded.
- [ ] Git commit SHA or baseline provenance recorded.
- [ ] Kernel version and CPU architecture recorded.
- [ ] CPU model and core count recorded.
- [ ] CPU governor set to `performance`, or omission documented.
- [ ] Turbo Boost disabled, or omission documented.
- [ ] NUMA binding applied where relevant, or marked not applicable.
- [ ] Competing load stopped, including browsers, IDEs, Docker containers, and background jobs.

## Thesis Budget Note

These controls are SHOULD-level for thesis runs: apply them whenever practical, and document which controls were applied for every benchmark result used in the thesis. A result with missing controls can still be useful, but the methodology section must state the missing controls and avoid overstating comparability.

## Valkey Methodology Reference

Valkey's benchmark methodology emphasizes controlled hardware, stable environments, and careful interpretation of benchmark results. Use it as the external methodology reference when explaining GoCache's Redis-compatible benchmark setup: <https://valkey.io/topics/benchmark/>.
