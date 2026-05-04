---
title: Architecture Decision Records
description: Index of architectural decisions captured for gocache — Nygard-format ADRs, one per significant decision
status: living
last_updated: 2026-05-03
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
| [0006](0006-builtin-vs-third-party-transport.md) | Built-in vs third-party plugin transport | proposed | 2026-05-03 |
| [0007](0007-embedded-persistence-plugin-self-config.md) | Embedded persistence plugins self-configure via viper | superseded by ADR-0008 | 2026-05-04 |
| [0008](0008-plugin-config-and-reload-contract.md) | Plugin config and reload contract is library-agnostic | proposed | 2026-05-04 |

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
