---
title: Architecture Decision Records
description: Index of architectural decisions captured for gocache — Nygard-format ADRs, one per significant decision
status: living
last_updated: 2026-05-28
related:
  - Server-Architecture
  - Performance
  - Plugins
---

# Architecture Decision Records

This directory captures architectural decisions as Nygard-format records. Each ADR is a short document describing one significant decision: the context that motivated it, the decision itself, the alternatives that were rejected, and the consequences (positive, negative, and the risks that come with them).

ADRs are the durable record of the *why* behind the code. They live alongside the source so future contributors can read them without context-switching to a separate doc system.

## Lifecycle

```
proposed → accepted → [deprecated | superseded by ADR-NNNN]
```

- **proposed** — under discussion; the matching code may not be merged yet. The PR that introduces the ADR is the review surface.
- **accepted** — in effect and being followed. Flipped from `proposed` once the matching contract / implementation lands.
- **deprecated** — no longer relevant (e.g. the feature was removed). Kept for historical record.
- **superseded** — a newer ADR replaces this one. The replacement link is mandatory.

## Index

| ADR | Title | Status | Date |
|-----|-------|--------|------|
| [0001](0001-persistence-as-pluggable-log-snapshot.md) | Persistence as pluggable log+snapshot+LSN | accepted | 2026-05-03 |
| [0002](0002-source-sink-contract.md) | Source/Sink contract with BootMode trichotomy | accepted | 2026-05-03 |
| [0003](0003-mutation-feed-and-fsync.md) | Mutation feed: group commit + fsync policy | accepted | 2026-05-03 |
| [0004](0004-command-namespacing.md) | Persistence command namespacing | proposed | 2026-05-03 |
| [0005](0005-snapshot-wire-and-file-format.md) | Snapshot wire and file format | accepted | 2026-05-03 |
| [0006](0006-builtin-vs-third-party-transport.md) | Built-in vs third-party plugin transport | accepted | 2026-05-03 |
| [0007](0007-embedded-persistence-plugin-self-config.md) | Embedded persistence plugins self-configure via viper | superseded by ADR-0008 | 2026-05-04 |
| [0008](0008-plugin-config-and-reload-contract.md) | Plugin config and reload contract is library-agnostic | accepted | 2026-05-04 |
| [0009](0009-rename-evaluator-to-pipeline.md) | Rename evaluator to pipeline | proposed | 2026-05-19 |
| [0010](0010-direct-shard-mutex-dispatch.md) | Replace channel dispatch with direct shard mutex | proposed | 2026-05-19 |
| [0011](0011-default-256-shards.md) | Increase default shard count to 256 | accepted | 2026-05-19 |
| [0012](0012-read-lock-dispatch.md) | Read-lock dispatch for read-only commands | accepted | 2026-05-19 |
| [0013](0013-pipeline-batch-coalescing.md) | Pipeline batch coalescing | accepted | 2026-05-19 |
| [0014](0014-pipelined-performance-limits.md) | Pipelined performance limits audit | accepted | 2026-05-19 |
| [0015](0015-mset-allocation-reduction.md) | MSET allocation reduction | accepted | 2026-05-19 |
| [0016](0016-aof-wire-and-file-format.md) | AOF wire and file format | accepted | 2026-05-21 |
| [0017](0017-mutation-replay-execution-path.md) | Mutation replay execution path | accepted | 2026-05-21 |
| [0018](0018-plugin-config-autonomy.md) | Plugin config autonomy (BindEnv + MergeFile) | proposed | 2026-05-24 |
| [0019](0019-unified-plugin-config-delivery.md) | Unified plugin config delivery | proposed | 2026-05-25 |
| [0020](0020-client-push-via-gcpc.md) | Client Push via GCPC | accepted | 2026-05-26 |
| [0021](0021-commons-package-layer.md) | Introduce commons/ Package Layer | accepted | 2026-05-26 |
| [0022](0022-modular-performance-budget.md) | Modular Performance Budget | accepted | 2026-05-28 |
| [0023](0023-lifecycle-otlp-and-runtime-instrumentation.md) | Lifecycle OTLP and Runtime Instrumentation Split | accepted | 2026-05-28 |
| [0024](0024-async-event-delivery-and-command-reaction-points.md) | Async Event Delivery and Command Reaction Points | proposed | 2026-05-29 |
| [0025](0025-connection-evaluators-and-connection-events.md) | Connection Evaluators and Connection Events | proposed | 2026-05-29 |
| [0026](0026-event-traffic-classes-and-backpressure.md) | Event Traffic Classes and Backpressure | proposed | 2026-05-29 |
| [0027](0027-event-replay-cursors-and-gaps.md) | Event Replay, Cursors, and Gaps | proposed | 2026-05-29 |

## Writing a new ADR

1. Copy `template.md` to `NNNN-decision-title.md` (next available number, kebab-case slug)
2. Fill in every section of the template — empty sections defeat the purpose
3. Add an entry to the index above
4. Open a PR — the PR review is where `proposed` ADRs get challenged
5. Flip to `accepted` once the matching code merges (separate commit, often in the same PR as the implementation)

## When to write one

| Worth recording | Not worth recording |
|---|---|
| Choice of database, transport, framework | Variable naming, formatting |
| Architecture pattern (microkernel, layered, event-driven) | Function refactor scope |
| API shape (REST vs gRPC, command namespacing) | Local optimizations |
| Auth strategy, encryption approach | Test framework choice (unless thesis-relevant) |
| Persistence model, data layout | Build-tool flag |

If the decision will outlive the people who made it and shapes how future code gets written, it belongs here.
