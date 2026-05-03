---
title: ADR-0001 Persistence as pluggable log+snapshot+LSN
description: Persistence moves from in-tree gob snapshot worker to a pluggable log+snapshot model with monotonic LSNs, allowing built-in and third-party providers
status: proposed
date: 2026-05-03
deciders: [witherxse]
related:
  - ADR-0002-source-sink-contract
  - ADR-0003-mutation-feed-and-fsync
  - ADR-0006-builtin-vs-third-party-transport
  - Server-Architecture
  - Performance
---

# ADR-0001: Persistence as pluggable log+snapshot+LSN

## Context

The current persistence implementation lives inside the server binary: a single goroutine drives gob-encoded snapshots, plus a planned in-tree AOF that never shipped. That layout has three problems. First, persistence is the dominant fixed RSS cost in a default deployment (~10× valkey at 1M keys, per the per-shard arc summary), and its memory shape is tied to the gob encoder's internals. Second, the snapshot/AOF dichotomy is hard-wired — users can pick `snapshot` or `aof` in YAML but cannot mix, replace, or extend the persistence layer. Third, third-party use cases like "boot from PostgreSQL", "stream mutations to Kafka", or "replicate to S3" have no extension point at all.

Prior art for the durability shape: etcd / Raft (log + periodic snapshot, both keyed by Raft index), PostgreSQL (WAL + base backup, LSN-keyed), Redis 7 (multi-part AOF — base + incremental files, replication-id-keyed), WiredTiger (pluggable storage `Env` for source-of-truth backends), Cassandra (commitlog + memtable flush + sstable). All five share the same primitives: an append-only mutation log, periodic full snapshots, and a monotonic position cursor that ties them together.

## Decision

Persistence becomes a pluggable subsystem keyed by **log + snapshot + LSN** (Log Sequence Number). The server emits a totally-ordered stream of mutations, each tagged with a monotonic 64-bit LSN; persistence providers consume that stream however they like (durable log, replication channel, archive). On boot, providers replay state from a snapshot at LSN=N and re-apply mutations from N+1 forward. Built-in snapshot and AOF become embedded plugins implementing the same contract; third-party providers (Postgres, S3, Kafka, custom) implement the contract via the public `api/persistence/` surface.

## Alternatives Considered

### Alternative 1: Keep gob snapshot in-tree, add AOF in-tree

- **Pros**: Smallest diff. No new contract surface. Familiar to anyone who's read the codebase.
- **Cons**: Doesn't fix the RSS overhead. Doesn't enable third-party providers. The "pick snapshot or AOF" toggle stays single-choice. Future work to add replication / archival starts from the same in-tree dead end.
- **Why not**: This was the original Phase 1 plan and is exactly what the per-shard arc summary identifies as the *next* major bottleneck (default-deployment memory cost). Sinking effort here doesn't move the thesis line.

### Alternative 2: Pure WAL (no snapshots)

- **Pros**: Simplest provider contract — providers consume a single mutation stream. No bifurcated state. Matches Postgres logical decoding directly.
- **Cons**: Recovery time grows with log length. Without snapshots, every cold boot re-applies the full mutation history — unbounded for long-running deployments. Compaction has to live somewhere, which reintroduces the snapshot concept under a different name.
- **Why not**: Recovery time is bounded only by snapshot cadence. Pure-WAL works at Postgres scale because the underlying store is durable B-trees that don't need replay; for an in-memory cache, snapshots aren't optional.

### Alternative 3: Pure snapshot (no log)

- **Pros**: Matches the current gob model. Simple recovery: load the latest snapshot, done.
- **Cons**: Recovery loses everything between the last snapshot and the crash. AOF exists in Redis specifically because snapshot-only durability is unacceptable for many workloads. Third-party "stream mutations" use cases have no hook point.
- **Why not**: Re-implements the same gap that motivated Redis to add AOF in 2010. Not a step forward.

### Alternative 4: External durability service (e.g., always-on Kafka)

- **Pros**: Offloads durability entirely. Leaner server binary.
- **Cons**: Hard external dependency for a basic cache. Drops the "single binary, no infrastructure" property that makes gocache approachable. Forces a transport choice on every user.
- **Why not**: The contract should *allow* this (third-party plugin) without *requiring* it. Built-in providers must work standalone.

## Consequences

### Positive

- Built-in snapshot and AOF can ship without bloating the core. They live as embedded plugins (see ADR-0006), share the same contract as third-party providers, and can be selectively excluded at build time.
- Third-party providers (Postgres source, Kafka sink, S3 archive) are first-class. They plug into the same `api/persistence/` surface as built-ins.
- Recovery is bounded by snapshot cadence regardless of mutation rate. Long-running deployments don't accumulate unbounded replay time.
- LSN cursoring lets multiple providers coexist (e.g., snapshot to disk + replicate to S3) without each duplicating the mutation stream.
- The persistence layer's RSS shape is no longer hardwired to gob's encoder internals — see ADR-0005.

### Negative

- More contract surface to design, document, and version. The `api/persistence/` package becomes a public-stability target.
- Two-stage provider boot (snapshot replay → log replay) is more complex than gob's one-shot load. This is the cost of bounded recovery time.
- Coordinating multiple providers (primary + replicas) introduces ordering questions that single-provider gob never had.

### Risks

- **Risk**: The contract turns out to be too narrow once real third-party providers are written. **Mitigation**: Status `proposed` until the built-in snapshot and AOF plugins both ship against the contract — that's two very different implementations exercising the same surface. If they reveal gaps, the contract revises before flipping to `accepted`.
- **Risk**: LSN allocation under per-shard locking re-introduces the global-mutex bottleneck the per-shard arc removed. **Mitigation**: ADR-0003 discusses fsync batching at the LSN-allocation site; the mutation feed is intentionally async from the cache write path.
