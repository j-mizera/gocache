---
title: ADR-0007 Embedded persistence plugins self-configure via viper
description: Embedded persistence plugins register at init time, read their own configuration from the server's viper, and handle their own hot reload — server config and the api/persistence surface stay free of plugin-internal keys
status: accepted
date: 2026-05-04
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0002-source-sink-contract
  - ADR-0005-snapshot-wire-and-file-format
  - ADR-0006-builtin-vs-third-party-transport
---

# ADR-0007: Embedded persistence plugins self-configure via viper

## Context

ADR-0001 established that persistence is a pluggable subsystem and ADR-0002 split the contract into Source/Sink/Snapshotter. ADR-0005 specified the v1 binary snapshot format. The remaining question is *how the server discovers and configures the active persistence plugin*.

Earlier iterations exposed plugin internals through `api/config.PersistenceConfig`: a `SnapshotFormat` string field selecting `"gob"` or `"v1"`, plus a `SnapshotFile` field holding the on-disk path. Both leaked plugin-internal concerns into the public configuration surface plugins import. The microkernel premise of the persistence-as-plugin arc is the opposite — the core knows there *is* a snapshot capability; it does not know what file the plugin writes, what format it writes in, or what tuning knobs the plugin exposes.

The original feedback: *"plugins should be self configurable and self sufficient, server should only call their api but it's them who decide about their config, who decide about formatting, who decide about what they do, api persistance was suppoused to be enly thin api layer, abstraction between server and persistance plugin, but server doesn't decide about anything more than calling their methods, deciding priority, when would they be invoked"*.

Plus: *"why plugins cant use viper to handle hot reloads and define their config themselves?"* — pointing at the existing config loader as the obvious mechanism rather than a custom shim.

## Decision

Embedded persistence plugins register themselves with the persistence layer at package-init time, then self-configure by reading their own subsection of the server's viper. The server exposes the viper handle (`pkg/config.Viper`) and a reload multiplexer (`pkg/config.OnReload`). Plugins do everything else.

`api/persistence` carries only the contract surface:

```go
type SnapshotProvider interface {
    Name() string
    Build() (Source, Snapshotter, error)
}

RegisterSnapshotProvider(p)        // called from plugin's init()
SnapshotProviderRegistered() Provider  // returns nil if none compiled in
```

`api/config.PersistenceConfig` shrinks to server-orchestration only:

```go
type PersistenceConfig struct {
    LoadOnStartup    bool          // server: do we call BootInto at startup?
    SnapshotInterval time.Duration // server: how often does the scheduler tick?
}
```

A plugin's `init()` calls `RegisterSnapshotProvider(&provider{})`. The provider's `Build()` runs after `pkg/config.Load`, reads the plugin's own subsection (`plugins.config.<plugin-name>.*`), sets defaults, and returns the wired Source/Snapshotter. Hot reload is registered via `pkg/config.OnReload(fn)` — the plugin's callback re-reads its keys and updates its in-memory state (e.g. `Source.SetFilename`, `Snapshotter.SetFilename`).

Selection is compile-time: `cmd/server/main.go` blank-imports the desired plugin package. Exactly one provider per binary; a second registration panics with both plugin names so the misbuild surfaces at startup.

The `LOAD_SNAPSHOT` runtime command is removed. Its only justification was a debug/test-time reload, and it required the server to know the plugin's working directory (for path-traversal sanitization). That's plugin-internal knowledge. If a future deployment needs runtime reload, the right path is a plugin-provided command (microkernel pattern), not a core RESP command.

## Alternatives Considered

### Alternative 1: Format string in `api/config` (the rejected status quo)

- **Pros**: Simplest mechanically. Users edit one YAML key to switch backends.
- **Cons**: Adding a new persistence backend (Postgres replication, Kafka, S3 archive) means adding a string to `api/config`, which means the core knows about every plugin. Inverts the dependency we want — the contract surface should not enumerate implementations.
- **Why not**: Direct user feedback rejected it during PR review of #67/#68. The microkernel boundary belongs at the contract surface, not inside an enum of implementations.

### Alternative 2: Server pushes config blobs to plugins

A `Build(rawConfig map[string]any)` signature where the server reads YAML, finds the subsection by plugin name, and hands a typed blob to each plugin.

- **Pros**: Plugins don't need to import viper. Server stays as the single config reader.
- **Cons**: Server still has to know "plugin subsections live at `plugins.config.<name>`", and it has to define the schema for that map. The server is still the central authority on plugin config; the plugin is just a passive recipient.
- **Why not**: Doesn't go far enough. The user's feedback was *plugins decide about their config* — pushing a blob still has the server in the loop. Letting plugins read viper directly is the cleaner separation: the server merely exposes "here's the config source," and the plugin owns the rest.

### Alternative 3: Each plugin owns its own YAML file

`/etc/gocache/plugins/snapshot-v1.yaml`, with the plugin loading independently.

- **Pros**: Maximally isolated — plugin owns its file end to end.
- **Cons**: Multiple watchers, multiple parsers, fragmented operator UX. Operators with one logical service now manage N config files. Hot reload semantics differ per plugin. No single source of truth.
- **Why not**: Optimizes for a separation that hurts ops more than it helps architecture. The shared viper instance gives plugins their own subsections without the multi-file tax.

### Alternative 4: Build tags only (no runtime registry)

- **Pros**: Compile-time conditional inclusion, zero runtime cost.
- **Cons**: Build tags don't run any registration logic; the plugin still has to expose its types to `cmd/server/main.go`, which means `main.go` has to know the plugin's package name and constructor signatures. That re-creates the central-enumeration problem build tags were supposed to fix.
- **Why not**: Solves the wrong layer. Blank imports + `init()` registration combines compile-time selection with runtime resolution in one mechanism.

## Consequences

### Positive

- `api/config` no longer knows persistence implementation details. Adding a new snapshot or AOF backend is a new package + a new blank import in `main.go`; no core change.
- Plugins use viper natively — defaults, types, env var overrides, hot reload via fsnotify all come for free. No reinventing config infrastructure.
- The reload multiplexer (`pkg/config.OnReload`) lets multiple plugins react to the same config-file change independently without stepping on each other (viper's native `OnConfigChange` is single-callback).
- Pattern extends cleanly: `RegisterAOFProvider` for the AOF plugin, `RegisterReplicationSink` for replication, etc. Each capability gets its own narrow registration interface; plugins implement whichever they care about.
- Compile-time enforcement of "exactly one provider per binary" — the panic-on-double-register surfaces conflicts at startup, not silently.
- `LOAD_SNAPSHOT` removal eliminates the worst leak: a server-level command needing plugin-internal knowledge (working directory). If reload is needed later, it lands as a plugin command.

### Negative

- The build configuration becomes load-bearing for "which persistence backend ships." A deployment that swaps backends must rebuild — not a config flip. Acceptable for the project's audience (single-user / research / thesis); operators who want runtime backend switching would need something heavier.
- Operators who previously used `LOAD_SNAPSHOT` to reload a different snapshot file at runtime have to restart the server (or wait for a future plugin command).
- Plugins that want viper now depend on `pkg/config` — which is fine for embedded plugins (they live in `pkg/`) but means the embedded-vs-IPC layering rule (ADR-0006, still proposed) needs to acknowledge this dependency explicitly when those plugins physically move to `plugins/` later.

### Risks

- **Risk**: Two embedded snapshot plugins compiled in by accident → panic at startup. **Mitigation**: The panic message names both plugins so the misconfiguration is immediately obvious. Build hygiene (separate cmd/server build for each plugin, or a single canonical default) keeps this rare.
- **Risk**: No plugin compiled in → server runs without persistence and silently drops state on restart. **Mitigation**: `cmd/server/main.go` logs a `persistence: no snapshot plugin compiled in; snapshots disabled` warning at startup so the operator notices. Loud enough to catch a misbuild.
- **Risk**: A plugin's reload callback panics during fan-out and breaks subsequent plugins' callbacks. **Mitigation**: Future enhancement — wrap each callback invocation in a recover. Today the multiplexer trusts callbacks (matches the pattern in the rest of the embedded plugin system).
- **Risk**: `viper.Sub` (used by plugins to scope to their subsection) returns nil if the subsection doesn't exist; a careless plugin would NPE on first read. **Mitigation**: Plugins read keys via the full path (`v.GetString("plugins.config.snapshot-v1.file")`) rather than via `Sub`, so missing subsections gracefully return defaults.
