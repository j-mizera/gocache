---
title: ADR-0019 Unified plugin config delivery
description: Framework injects PluginConfig into every plugin type — embedded and IPC — so plugins never self-fetch config
status: proposed
date: 2026-05-25
deciders: [witherxse]
related:
  - ADR-0008
  - ADR-0018
  - Plugins
---

# ADR-0019: Unified plugin config delivery

## Context

GoCache has three plugin types, each consuming configuration differently:

| Plugin type | Example | Config delivery |
|---|---|---|
| Persistence (embedded) | AOF, Snapshot | `PluginConfig` injected via `Provider.Build(cfg, store)` |
| Embedded lifecycle | Crashdump, lifecycleotlp | Self-fetch via `config.PluginConfigFor(name)` in `ConfigLoaded` |
| IPC (external process) | Prometheus | `os.Getenv()` only — no access to server YAML |

This inconsistency has three costs:

1. **Embedded plugins call `config.PluginConfigFor` themselves.** The framework already knows the plugin's name — making the plugin repeat it is error-prone and violates inversion of control.
2. **IPC plugins have no access to YAML config at all.** Operators must set env vars for every knob; there is no way to manage IPC plugin config in the central `gocache.yaml`.
3. **No reload path for IPC plugins.** When the server hot-reloads config, embedded plugins see fresh values through their viper view, but IPC plugins remain on stale env vars until restarted.

## Decision

### Part 1: Embedded plugin injection

Change the `embedded.Plugin` interface:

```go
// Before
ConfigLoaded(ctx context.Context, cfg *config.Config) error

// After
ConfigLoaded(ctx context.Context, cfg *config.Config, pcfg config.PluginConfig) error
```

`ConfigLoadedAll` constructs the scoped view and injects it:

```go
func ConfigLoadedAll(ctx context.Context, cfg *config.Config) {
    for _, r := range registry {
        pcfg := config.PluginConfigFor(r.plugin.Name())
        invoke(ctx, r, "config_loaded", func() error {
            return r.plugin.ConfigLoaded(ctx, cfg, pcfg)
        })
    }
}
```

Callers of `ConfigLoadedAll` are unchanged — the extra parameter is internal.

### Part 2: IPC plugin config delivery via GCPC

Add a `config` field to `RegisterAckV1` so the server delivers the plugin's YAML config section at registration time:

```protobuf
message RegisterAckV1 {
    bool accepted = 1;
    string reason = 2;
    repeated string granted_scopes = 3;
    map<string, string> config = 4;
}
```

Add a `PluginConfigV1` message and envelope variant for hot-reload pushes:

```protobuf
message PluginConfigV1 {
    map<string, string> entries = 1;
}
// In EnvelopeV1 oneof:
PluginConfigV1 config_update = 90;
```

The SDK receives a `RemoteConfig` that implements `api/config.PluginConfig`, backed by the server-delivered map. Priority: `os.Getenv` (via `BindEnv`) > server values > local `SetDefault` values. Plugins that want to react to reload implement the new `ConfigPlugin` interface:

```go
type ConfigPlugin interface {
    Plugin
    OnConfigReload(cfg config.PluginConfig)
}
```

Server-side: `pkg/config.FlatPluginConfig(name)` reads all keys under `plugins.config.<name>` from viper and returns them as `map[string]string`. The manager calls this at registration to populate the ack, and registers `OnPluginReload` to push updates.

## Alternatives Considered

### Alternative 1: Plugin-level viper instance per IPC plugin

Ship the full viper serialization (JSON blob) over GCPC and deserialize into a local viper on the plugin side.

- **Pros**: Full viper feature parity (nested keys, type coercion, AllSettings).
- **Cons**: Pulls viper into the SDK (it's currently server-only). Couples the wire format to viper's serialization model. Over-engineering for what is typically 3-5 flat keys.
- **Why not**: `map[string]string` with `strconv` is sufficient for the config surface IPC plugins actually need, and keeps the SDK dependency-light.

### Alternative 2: Keep self-fetch for embedded plugins

Only do Part 2 (IPC delivery), leave embedded plugins calling `config.PluginConfigFor` themselves.

- **Pros**: Smaller diff for Part 1.
- **Cons**: Inconsistent — persistence plugins get injection, embedded lifecycle plugins self-fetch. Two patterns for the same concept in the same process.
- **Why not**: The interface change is mechanical (one parameter addition) and makes all in-process plugins consistent.

## Consequences

### Positive

- One config pattern for all plugin types: the framework injects, the plugin receives.
- IPC plugins gain access to server YAML config without env-var workarounds.
- Hot-reload pushes to IPC plugins — no restart needed for config changes.
- Embedded plugins no longer need to know their own registered name for config lookup.

### Negative

- `embedded.Plugin` interface breaks — all implementations must add the `pcfg` parameter. Mechanical but touches every embedded plugin.
- GCPC protocol gains a new field (`config` on ack) and a new message type (`PluginConfigV1`). Wire-compatible (additive), but external plugins compiled against the old proto won't see config until recompiled.
- `RemoteConfig` in the SDK is a new type to maintain.

### Risks

- **Risk**: `FlatPluginConfig` flattens nested YAML keys to dot-separated strings. Deep nesting (`a.b.c`) in plugin config could collide with the dot separator. **Mitigation**: Plugin config sections are conventionally flat (1 level deep). Document the flat-key convention.
- **Risk**: Reload push races with in-flight command handling. **Mitigation**: `RemoteConfig.Replace` swaps the backing map atomically (sync.Mutex). Reads during swap see either old or new values, never partial.
