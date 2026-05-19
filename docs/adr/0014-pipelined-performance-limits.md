---
title: ADR-0014 Pipelined performance limits audit
description: Document the structural performance ceiling of Go's sync.Mutex vs single-threaded C event loops and the resulting GoCache-vs-Valkey tradeoff
status: accepted
date: 2026-05-19
deciders: [witherxse]
related:
  - Performance
  - ADR-0010-direct-shard-mutex-dispatch
  - ADR-0011-default-256-shards
  - ADR-0012-read-lock-dispatch
  - ADR-0013-pipeline-batch-coalescing
---

# ADR-0014: Pipelined performance limits audit

## Context

After implementing three dispatch optimizations (ADR-0011 256 shards, ADR-0012 read-lock dispatch, ADR-0013 pipeline batch coalescing), benchmarks show GoCache wins 14/15 standard commands but loses 8/15 pipelined commands vs Valkey. Pipelined collection gaps closed from 34-58% to 1-18%, and MSET from +160% to +64%.

The question: can the remaining pipelined write gap (10-18% on single-key, +64% on MSET) be closed further, or is it structural?

## Decision

The remaining pipelined write gap is the irreducible cost of Go's `sync.Mutex` and is accepted as a design tradeoff. No further optimization is pursued for single-key pipelined writes. MSET's +64% gap has an actionable component (per-call allocations and `TouchedShards` map fallback at 256 shards) that will be addressed separately.

### What's structural (not fixable in safe Go)

**`sync.Mutex` base cost**: 50-150ns per acquire+release, even uncontended. `Lock()` issues `LOCK CMPXCHG` (full memory fence). Valkey's single-threaded event loop pays 0ns for serialization. With 256 shards eliminating contention, the remaining cost is this floor.

**Go's netpoller uses `epoll`**: Valkey 8's +230% throughput came from `io_uring` (PRs #758, #763). Go's network stack has no `io_uring` path without CGo.

**No memory prefetch**: Valkey PR #861 reduced `lookupKey` cost 80% via `PREFETCH`. Go doesn't expose CPU prefetch instructions.

**Goroutine scheduling**: Mutex-blocked goroutines get rescheduled, possibly to a different CPU (L1/L2 invalidation). Valkey never context-switches during a pipeline batch.

**GC pauses**: Sub-millisecond pauses contribute to p99 gap (1.5-2ms vs 0.5-0.8ms).

### What was evaluated and rejected

| Approach | Why rejected |
|----------|-------------|
| `unsafe` + lock-free CAS | Cache entries are multi-field structs (value + TTL + LRU + size); can't CAS atomically. COW means O(N) allocation per write. Loses race detector and memory safety — defeats choosing Go. |
| Thread-per-core (Seastar) | Go goroutines migrate between OS threads; no stable CPU affinity. Would require `runtime.LockOSThread()` defeating goroutine scheduling. |
| `xsync.RBMutex` | External dependency. At 256 shards per-shard read contention is near zero. Relevant at 64+ cores / NUMA, not current scale. |
| Flat combining | Requires thread-local publication slots incompatible with goroutine migration. Complex, fragile. |

### What GoCache gets in return

- Standard (non-pipelined): 20%+ faster on 14/15 commands — parallel goroutines beat serial event loop
- Process-isolated plugins — crashing plugin can't crash core
- Memory safety — no use-after-free, no buffer overflows, race detector available

## Alternatives Considered

### Alternative 1: Accept gap and document it

- **Pros**: No code changes, honest thesis narrative
- **Cons**: Leaves MSET's +64% gap which has fixable allocation overhead
- **Why not**: MSET has actionable per-call allocation waste unrelated to the structural mutex cost

### Alternative 2: Rewrite hot path with unsafe atomics

- **Pros**: Could eliminate mutex cost entirely for simple SET/INCR
- **Cons**: Loses Go's safety guarantees, race detector becomes useless, ABA bugs, 10x maintenance burden
- **Why not**: The thesis argues safe extensibility and performance coexist. unsafe atomics undermine the thesis statement itself.

## Consequences

### Positive

- Clear, measurable thesis narrative: "pay 10-18% pipelined writes, gain 20%+ standard + safety"
- Engineering effort redirected to MSET allocation fix (actionable) instead of chasing structural limits
- ADR serves as permanent record preventing future re-investigation of the same ceiling

### Negative

- Pipelined write-heavy benchmarks will always show Valkey ahead by 10-18%
- MSET will remain the worst-performing command proportionally even after allocation fixes

### Risks

- Future Go versions may expose `io_uring` or improve mutex implementation, changing the calculus. Mitigation: this ADR records the state as of Go 1.25 / 2026-05. Re-benchmark if Go's sync primitives change significantly.
