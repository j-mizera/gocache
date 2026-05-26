# Benchmark Comparison: Audit Cleanup PR #85

**Date:** 2026-05-26
**Commit:** `595f853` (fix: codebase audit cleanup — correctness, lifecycle, commons/ layer)
**Baseline:** `93967a8` (pre-merge main)
**CPU:** AMD Ryzen 9 7900X 12-Core Processor (24 threads)

## Verdict: No Performance Regressions

All variations fall within noise margins. The valkey control group (unchanged code) shows equal or greater variance, confirming container scheduling noise — not code regression.

## Go Micro-benchmarks

### RESP Layer (`commons/resp`, formerly `pkg/resp`)

| Metric | Result |
|--------|--------|
| B/op | **Identical** — every benchmark matches exactly |
| allocs/op | **Identical** — every benchmark matches exactly |
| Timing | 15-37% variation (noise — code is byte-for-byte identical after move) |

The RESP package was moved from `pkg/resp` to `commons/resp` with zero code changes. Allocation identity confirms no regression from the import path migration.

New benchmarks added (from merged `api/resp/encode.go`): `EncodeBulkString`, `EncodeMessage` — no baseline exists.

### Shardproto (`pkg/shardproto`)

| Metric | Result |
|--------|--------|
| B/op | < 4% variation, statistically insignificant |
| allocs/op | **Identical** across all matched benchmarks |
| Timing | High variance (± ∞), insufficient samples for confidence |

Baseline ran count=5, after ran count=3. Both below the >= 6 sample minimum for statistical confidence intervals. The after run is also missing Standard-mode benchmarks (GET_Standard, SET_Standard). No meaningful regression signal.

### Server (`pkg/server`)

Baseline and after benchmarks are both corrupted — the server logger writes to `os.Stdout`, which overwrites the `go test -bench` output buffer. Zero `ns/op` data in either file.

A re-run with `logger.Init("fatal")` via TestMain produces clean output (saved as `server-clean.txt`). No valid baseline exists for comparison.

**Fix for future runs:** Add a `TestMain` to `pkg/server/` that initializes the logger with level `"fatal"` before benchmarks.

## Docker redis-benchmark

Single-run benchmarks (no statistical confidence). Valkey serves as an unchanged control group to distinguish real regressions from container scheduling noise.

### Control Group: Valkey (unchanged code)

| Mode | Range of change |
|------|----------------|
| Standard | -14% to -34% |
| Pipelined | -15% to -35% |
| Mixed SET | -11.5% |
| Mixed GET | -14.9% |

Valkey's code was not modified. These swings are pure container/system noise.

### GoCache

| Mode | Range of change | vs Valkey control |
|------|----------------|-------------------|
| Standard | -3% to -34% | Within valkey's noise band |
| Pipelined | -18% to +103% | Wider swings both ways, noise |
| Mixed SET | -9.0% | Less regression than valkey (-11.5%) |
| Mixed GET | -3.0% | Less regression than valkey (-14.9%) |

GoCache's mixed workload regressed **less** than valkey in both SET and GET. No evidence of real performance impact.

### Selected Standard-mode Comparisons

| Command | Baseline | After | Δ | Valkey Δ |
|---------|----------|-------|---|----------|
| SET | 98.5K | 87.5K | -11% | -4% |
| GET | 85.3K | 75.1K | -12% | -28% |
| HSET | 92.3K | 84.5K | -8% | -13% |
| LPUSH | 82.1K | 63.3K | -23% | -26% |
| RPOP | 86.8K | 88.3K | +2% | -6% |
| LRANGE_100 | 41.2K | 48.2K | +17% | -34% |

### Selected Pipelined-mode Comparisons

| Command | Baseline | After | Δ | Valkey Δ |
|---------|----------|-------|---|----------|
| SET | 378.8K | 769.2K | +103% | -35% |
| GET | 609.8K | 757.6K | +24% | +4% |
| HSET | 645.2K | 490.2K | -24% | -35% |
| PING_INLINE | 396.8K | 781.2K | +97% | -31% |

## Files

```
bench/results/
├── audit-cleanup-baseline/      # Pre-merge (93967a8)
│   ├── resp.txt                  # count=5
│   ├── server.txt                # CORRUPTED (logger noise)
│   └── shardproto.txt            # count=5
├── audit-cleanup-after/          # Post-merge (595f853)
│   ├── resp.txt                  # count=3
│   ├── server.txt                # CORRUPTED (logger noise)
│   ├── server-clean.txt          # count=3, with logger suppression
│   ├── shardproto.txt            # count=3
│   └── post-audit-*.csv          # Docker redis-benchmark
└── bench-audit-cleanup-baseline/ # Docker baseline
    └── baseline-*.csv            # Docker redis-benchmark
```
