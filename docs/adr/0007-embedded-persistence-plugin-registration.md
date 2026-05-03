---
title: ADR-0007 Embedded persistence plugins register via init()
description: Snapshot and AOF backends register themselves with the persistence layer via init-time hooks; main.go selects them via blank imports rather than a config string
status: accepted
date: 2026-05-03
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0002-source-sink-contract
  - ADR-0005-snapshot-wire-and-file-format
  - ADR-0006-builtin-vs-third-party-transport
---

# ADR-0007: Embedded persistence plugins register via init()

## Context

ADR-0001 established that persistence is a pluggable subsystem and ADR-0002 split the contract into Source/Sink/Snapshotter. ADR-0005 specified the v1 binary snapshot format. The remaining question is *how the server discovers which persistence plugin is active*.

The first cut wired this through `api/config.PersistenceConfig.SnapshotFormat` — a string field the user set to `"gob"` or `"v1"`, with `cmd/server/main.go` switching on it. That works mechanically but leaks a plugin-internal concern (which on-disk format the server uses) into the public configuration surface. The microkernel premise of the persistence-as-plugin arc is the opposite: the core should not know plugin implementation details. A plugin's name, configuration knobs, and on-disk format belong to the plugin, not to `api/config`.

ADR-0006 (still proposed) distinguishes embedded plugins (compile-time, in-process) from IPC plugins (separate process via Unix socket + GCPC). Snapshot and AOF are the canonical embedded cases — performance-critical, tightly coupled to the cache's memory shape, and not interesting to swap at runtime. They need a registration mechanism that matches their compile-time nature without bouncing through a config string.

## Decision

Embedded persistence plugins register themselves with the persistence layer at package-init time. `api/persistence` exposes a registration function (`RegisterSnapshotProvider`); each plugin's `init()` calls it. The server selects which plugin is active by *blank-importing* the desired plugin package in `cmd/server/main.go`. Exactly one snapshot provider may be registered per binary; a second registration panics at init with both plugin names.

The same pattern will apply to AOF when it lands (`RegisterAOFProvider`). `api/config` carries no format/format-name strings. The runtime `LOAD_SNAPSHOT` command keeps a separate format-detection helper (`pkg/persistence.DetectFormat`) so an operator can load an archived gob file even on a binary that compiled in only the v1 plugin — that's an operator convenience, not a configuration knob.

## Alternatives Considered

### Alternative 1: Format string in `api/config` (the rejected status quo)

- **Pros**: Simplest mechanically. Users edit one YAML key to switch backends. Config is a single source of truth.
- **Cons**: `api/config` is the public surface plugins import; putting `"gob" | "v1"` strings there couples every plugin to the central enumeration of *every* persistence backend that ever ships. Adding a Postgres replication source means adding a string to `api/config` — which means the core knows about the Postgres plugin. Inverts the dependency we want.
- **Why not**: The user feedback during PR-C review was explicit — *"snapshot format should be specified by plugins themselves, not by general api server config, that was the whole point"*. The microkernel boundary belongs at the contract surface, not inside an enum of implementations.

### Alternative 2: Manifest-driven runtime selection

- **Pros**: One YAML key (`persistence.provider: snapshot-v1`) selects the active plugin. All plugins compile in; deployment chooses at startup. Matches Kafka Connect / WiredTiger Env style.
- **Cons**: The plugin choice is a build-time fact for any deployment in practice — no operator picks Postgres replication on Tuesday and snapshot on Wednesday. Adding runtime indirection for a static fact pays cost without buying anything. Also: every deployed binary carries every plugin's code, even ones it never uses.
- **Why not**: Optimizes for a flexibility nobody needs and pays for it in binary size and startup complexity.

### Alternative 3: Build tags

- **Pros**: Compile-time selection, zero runtime cost. Standard Go idiom for "include feature X" conditional compilation.
- **Cons**: Build tags don't run any registration logic; the plugin still has to expose its types to `cmd/server/main.go`, which means `main.go` has to know the plugin's package name and constructor signatures. That re-creates the central-enumeration problem build tags were supposed to fix.
- **Why not**: Solves the wrong layer — the conditional inclusion is fine, but selecting *which* plugin is active still has to be a runtime resolution. Blank imports + `init()` registration combines both: the import is the conditional inclusion, the init is the registration. One mechanism does both jobs.

### Alternative 4: Plugin discovery via reflection

- **Pros**: No explicit registration call. Plugins drop into a known directory; the loader scans for them.
- **Cons**: Reflection is the wrong tool for compile-time configuration. Adds a runtime walk for a fact that's literally encoded in the import graph. Errors surface at startup as obscure reflection misses rather than at compile time as missing imports.
- **Why not**: All cost, no benefit relative to blank imports.

## Consequences

### Positive

- `api/config` no longer knows persistence implementation details. Adding a new snapshot or AOF backend is a new package + a new blank import in `main.go`; no core change.
- Compile-time enforcement of "exactly one provider per binary" — the panic-on-double-register surfaces conflicts at startup, not silently.
- Configuration stays minimal: `persistence.snapshot_file`, `persistence.snapshot_interval`, `persistence.load_on_startup` are the user-facing knobs. The plugin's identity is a build-time fact.
- Pattern extends cleanly: `RegisterAOFProvider` for the AOF plugin, `RegisterReplicationSink` for replication, etc. Each capability gets its own narrow registration interface; plugins implement whichever they care about.
- Forward-compatible with ADR-0006's eventual physical move to `plugins/`. The registration interface is the same wherever the implementation file lives.

### Negative

- The build configuration becomes load-bearing for "which persistence backend ships." A deployment that swaps backends must rebuild — not a config flip. Acceptable for the project's audience (single-user / research / thesis); operators who want runtime backend switching would need something heavier.
- Tests that need a non-default provider must arrange their own registration (or work directly with the plugin's constructors, bypassing the registry). The default test path uses the constructors directly; only integration tests touching `cmd/server/main.go` would care.

### Risks

- **Risk**: Two embedded snapshot plugins compiled in by accident → panic at startup. **Mitigation**: The panic message names both plugins so the misconfiguration is immediately obvious. Build hygiene (separate cmd/server build for each plugin, or a single canonical default) keeps this rare.
- **Risk**: No plugin compiled in → server runs without persistence and silently drops state on restart. **Mitigation**: `cmd/server/main.go` logs a `persistence: no snapshot plugin compiled in; snapshots disabled` warning at startup so the operator notices. This is the intended behaviour for ephemeral dev runs but should be loud enough to catch a misbuild.
- **Risk**: A future architectural change (e.g., an embedded plugin that *also* needs to serve a different responsibility) wants composition rather than single-registration. **Mitigation**: The registration model is per-capability — each capability has its own narrow interface and its own register/lookup. A plugin can implement multiple capabilities; the binary holds at most one provider per capability. Composition lives in the plugin package, not in the registry.
