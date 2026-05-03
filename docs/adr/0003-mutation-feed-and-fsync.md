---
title: ADR-0003 Mutation feed — group commit and fsync policy
description: Mutations flow to Sinks via a buffered group-commit channel (1ms or 64KB triggers); fsync is a per-Sink policy with three levels (Always, EverySec, No)
status: accepted
date: 2026-05-03
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0002-source-sink-contract
---

# ADR-0003: Mutation feed — group commit and fsync policy

## Context

ADR-0002 establishes that `Sink` implementations consume the mutation feed during steady-state operation. The hot question is *how* mutations flow to sinks: synchronously (every command waits for every sink to acknowledge), asynchronously (fire-and-forget, sink runs on its own clock), or batched somewhere in between.

The constraints. The cache write path is now per-shard locked (default N=8) and serves ~0.7-0.95× valkey on pipelined writes; making every command block on a sink would erase that gain. But pure async with no backpressure means a slow sink silently desynchronizes from the cache, and durability claims become aspirational. There's also a cross-cutting decision: durability semantics for built-in sinks (snapshot, AOF) need to map cleanly to user-facing config — Redis has trained users to expect `appendfsync: always | everysec | no` and the corresponding latency/durability trade-offs.

Prior art: Redis MP-AOF uses group commit with the same three fsync policies. PostgreSQL synchronous_commit has a similar trichotomy (on / off / local). etcd batches WAL appends at the Raft batching boundary. All three converge on the same shape: buffer mutations, flush on a time-or-size trigger, fsync per a policy that the user chose.

## Decision

The mutation feed flows to sinks through a **group-commit channel** with two trigger conditions:

- **Time trigger**: 1 ms since the last flush
- **Size trigger**: 64 KB of buffered mutations

Whichever fires first flushes the buffer to all registered sinks. This decouples the cache write path from sink latency: commands don't wait for sinks; sinks see batched writes that amortise their own per-flush cost.

Fsync is a **per-sink policy** with three levels matching Redis convention:

- **`FsyncAlways`** — fsync after every flushed batch. Highest durability, highest latency. Equivalent to Redis `appendfsync always`.
- **`FsyncEverySec`** — fsync at most once per second, on a separate goroutine. Loses up to 1s of writes on crash. Default for built-in AOF. Equivalent to Redis `appendfsync everysec`.
- **`FsyncNo`** — no explicit fsync; rely on the OS page cache flush. Loses up to 30s of writes on crash. For dev or non-durable workloads. Equivalent to Redis `appendfsync no`.

Each sink declares its policy at registration; the coordinator dispatches per sink without forcing one global policy across all sinks.

## Alternatives Considered

### Alternative 1: Per-mutation synchronous dispatch

- **Pros**: Strongest durability semantics — every command's success implies every sink acknowledged. No buffering to lose on crash.
- **Cons**: Erases the per-shard locking gain. Pipelined writes drop from ~0.85× valkey to whatever the slowest sink can sustain. A slow third-party sink (e.g., Postgres) would tank cache throughput.
- **Why not**: The cache's value proposition is in-memory speed; persistence shouldn't dictate it. Users who want per-mutation durability run a database, not a cache.

### Alternative 2: Pure async fire-and-forget

- **Pros**: Maximum cache throughput — sinks see writes when they see them.
- **Cons**: No backpressure means a slow sink silently falls behind. On crash, the gap between cache state and sink state is unbounded. Durability claims become unverifiable.
- **Why not**: Without backpressure the system can't surface "your sink is failing" to users until something else crashes. The 1ms / 64KB triggers give enough batching to amortise sink overhead while keeping the buffer bounded.

### Alternative 3: Single global fsync policy

- **Pros**: Simpler config (one knob). Matches Redis 6.x exactly.
- **Cons**: Mixed-sink deployments can't express "fsync local AOF every second AND replicate to remote S3 with no fsync". Forces every sink to the strictest policy in the set.
- **Why not**: Once we accept that multiple sinks coexist (ADR-0001), each sink's durability needs are independent. Per-sink policy is a small cost on top of per-sink registration.

### Alternative 4: Different group-commit triggers (e.g., 10ms, 256KB)

- **Pros**: Larger batches → lower per-flush amortised cost.
- **Cons**: Larger time window → larger crash window for `FsyncEverySec`. 1ms is the boundary where most burst traffic gets coalesced without adding meaningful latency to the rare under-1ms-pipeline case. 64KB is the standard kernel write-coalescing block.
- **Why not**: 1ms / 64KB are Redis 7's actual values, validated against real workloads. Reasonable defaults; tunable per-sink if a deployment has different needs.

## Consequences

### Positive

- Cache write path stays decoupled from sink latency — pipelined throughput is unaffected by sink choice.
- Group commit amortises per-flush overhead, so sinks like AOF (one fsync per batch) and S3 (one PUT per batch) both benefit.
- Per-sink fsync policy lets users mix durability needs (e.g., `FsyncEverySec` for local AOF + `FsyncNo` for archival S3 sink).
- Maps cleanly to Redis-trained user expectations.

### Negative

- A bounded-latency upper bound on writes (1ms group-commit window) means the durability-vs-latency trade-off is the same for every sink unless the user explicitly tunes it. Most workloads won't notice; some latency-sensitive ones will.
- Sink errors during a group commit are batched too — one failing sink in a batch means the whole batch's error attribution is fuzzier than per-mutation dispatch would give.
- Slow sinks fill the buffer and eventually block the producer (the cache write path). Backpressure is real but bounded; users get a knob (buffer size) but not unlimited absorption.

### Risks

- **Risk**: A single misbehaving sink stalls all sinks via shared group-commit batching. **Mitigation**: Each sink consumes from its own buffer; the group-commit batches are per-sink, not shared across sinks. A slow Postgres sink doesn't slow down a healthy AOF sink.
- **Risk**: `FsyncEverySec` background goroutine drift on heavy load means the "1 second" promise is actually 1.5-2 seconds. **Mitigation**: Same risk Redis has; documented in user-facing config so expectations match implementation.
- **Risk**: Buffer fills under a write spike, causing the write path to block. **Mitigation**: Buffer size is configurable. The fact that backpressure exists is a feature, not a bug — silent unbounded buffering would be worse.
