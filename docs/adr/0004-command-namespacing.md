---
title: ADR-0004 Persistence command namespacing
description: Universal persistence commands (SAVE, BGSAVE, LASTSAVE, BGREWRITEAOF) stay in the core RESP namespace; third-party connector commands go in the REX namespace
status: proposed
date: 2026-05-03
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0006-builtin-vs-third-party-transport
  - GCPC
---

# ADR-0004: Persistence command namespacing

## Context

Once persistence is pluggable (ADR-0001), the question becomes: where do persistence-related *commands* live in the protocol surface? Redis has a small set of universal persistence commands (`SAVE`, `BGSAVE`, `BGREWRITEAOF`, `LASTSAVE`) that every client library knows about. Pluggable persistence introduces a tension: users expect these commands regardless of which provider they're using, but pluggable providers might also want their own commands (`POSTGRES.SYNC`, `S3.ARCHIVE`, etc.).

The codebase has a clean rule for plugin-introduced commands: they go in the **REX namespace** with a `PLUGIN.COMMAND` shape (e.g., `OBSERVE.STATS`). REX is a RESP3 superset and cleanly separates plugin surface from core protocol. The default-routing rule prevents a plugin from claiming `SET` or `GET`.

The decision splits two cases. Case A: commands that are universal across persistence providers (start a save, ask when the last save was, request an AOF rewrite). Case B: commands that are specific to one provider (Postgres sync state, S3 manifest, custom replication position). Mashing both into REX would force users to learn `PERSISTENCE.SAVE` instead of `SAVE` — a regression for users expecting Redis compatibility. Mashing both into the core namespace would require every plugin author to fight the core for keyspace.

## Decision

Persistence commands are namespaced by **whether they are universal or provider-specific**:

- **Universal commands stay in core RESP namespace**, dispatched by the engine to the configured primary persistence provider:
  - `SAVE` — synchronous full snapshot
  - `BGSAVE` — asynchronous full snapshot
  - `LASTSAVE` — Unix timestamp of last successful snapshot
  - `BGREWRITEAOF` — async AOF rewrite (when AOF sink is registered)
- **Provider-specific commands go in the REX namespace** with the plugin's chosen prefix (e.g., `S3.ARCHIVE`, `POSTGRES.SYNC.STATUS`). These are surfaced to clients only when the corresponding plugin is loaded.

The engine routes universal commands to the registered "primary" sink for that command type (one snapshot-capable sink for SAVE/BGSAVE, one AOF-capable sink for BGREWRITEAOF). Sinks declare which universal commands they handle at registration; if no sink claims a universal command, it returns the appropriate error (`PERSISTENCE_DISABLED`).

## Alternatives Considered

### Alternative 1: All persistence commands in REX namespace

- **Pros**: Clean separation — core RESP stays minimal, every persistence command goes through the same plugin surface.
- **Cons**: Breaks Redis client compatibility. `redis-cli SAVE` would have to become `redis-cli PERSISTENCE.SAVE`. Every Redis client library would need patching to call gocache. The "Redis-compatible" property in the project pitch is broken at the protocol surface.
- **Why not**: Redis compatibility is a top-level project goal; the universal commands are exactly the surface where Redis compatibility matters most.

### Alternative 2: All persistence commands in core RESP namespace

- **Pros**: No namespace split to learn.
- **Cons**: Every plugin author who wants a custom persistence command (Postgres source state, S3 manifest, etc.) has to fight the core for command names. The microkernel philosophy explicitly rejects this — extensions must be namespaced so the core stays small.
- **Why not**: REX exists for exactly this reason — it's the discriminator between "core surface" and "plugin surface". Skipping it for persistence-specific commands violates the rule for no good reason.

### Alternative 3: Universal commands as REX with core aliasing

- **Pros**: Maximally consistent — every persistence command is a REX command, but `SAVE` aliases to `PERSISTENCE.SAVE` for Redis compatibility.
- **Cons**: Aliasing adds a hop in the dispatch path for every command. The aliasing layer needs its own protocol surface and edge cases. `MONITOR` output, error messages, and command completion all have to handle the alias.
- **Why not**: Aliasing solves a problem we don't have — the universal commands' definitions are the same regardless of which sink answers them. Aliasing adds ceremony for no benefit.

### Alternative 4: Core RESP namespace with provider-name prefix at command level

- **Pros**: One namespace; provider-specific commands naturally namespaced by provider name (`AOF.REWRITE` instead of `BGREWRITEAOF`).
- **Cons**: Same problem as Alternative 1 for universal commands — `SAVE` becomes `SNAPSHOT.SAVE` and Redis compatibility breaks. Plus the existing REX namespace is already the canonical extension point; introducing a parallel one duplicates the concept.
- **Why not**: Reuses the existing REX mechanism for the right thing (extension-specific commands) and keeps the core surface stable.

## Consequences

### Positive

- Redis client libraries see expected command names for the Redis-compatible commands.
- Plugin authors get a clean extension point for provider-specific commands without fighting the core.
- The "primary sink for SAVE" routing is explicit, so users with multiple sinks aren't surprised by which one a plain `SAVE` hits.
- REX permission scopes already exist and apply to provider-specific commands automatically.

### Negative

- Two routing rules to document: universal commands route by command name to a registered primary; provider commands route by REX prefix to the plugin.
- Users with multiple snapshot-capable sinks have to configure which is "primary" for SAVE. If they don't, gocache picks deterministically (registration order) and that surprises some users.

### Risks

- **Risk**: Two plugins both claim "primary AOF sink" capability, and the user expects both to be hit by `BGREWRITEAOF`. **Mitigation**: Configuration must declare which sink is primary for each universal command; if both claim by default, registration fails fast at startup with a clear error. No silent winner-takes-all.
- **Risk**: A future Redis adds a new universal persistence command that gocache hasn't routed. **Mitigation**: Universal commands are a closed list maintained alongside the protocol layer. Adding one is a contract change with its own ADR — not a silent degradation.
