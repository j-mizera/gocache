---
title: ADR-0008 Plugin config and reload contract is library-agnostic
description: Embedded plugins receive a typed PluginConfig view and an optional ReloadHandler — the server's choice of config library (viper) is fully internal and never leaks across the api/ boundary
status: proposed
date: 2026-05-04
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0002-source-sink-contract
  - ADR-0006-builtin-vs-third-party-transport
  - ADR-0007-embedded-persistence-plugin-self-config
supersedes:
  - ADR-0007-embedded-persistence-plugin-self-config
---

# ADR-0008: Plugin config and reload contract is library-agnostic

## Context

ADR-0007 established that embedded persistence plugins self-configure rather than receiving plugin-specific keys through `api/config`. The mechanism it landed on was direct viper exposure: `pkg/config.Viper() *viper.Viper` and `pkg/config.OnReload(func(*viper.Viper))`. Plugins imported `github.com/spf13/viper`, called `v.SetDefault` / `v.GetString` directly, and received `*viper.Viper` in their reload callback.

That fixes the *config-key* leak (no more `SnapshotFormat` or `SnapshotFile` in `api/config`) but introduces a *config-library* leak. Plugins now know the server uses viper; they import it and type their callback parameters with it. The server's choice of config library is supposed to be an implementation detail. The user's framing: *"plugins shouldn't know what server uses, if it's viper or anything else, they just receive event that configuration has changed […] library names do not define functions, functionality is just config"*.

The plugin contract surface should name **roles**, not **libraries**. Today the role is "give me the value of this scoped key" and "tell me when config has changed" — both expressible without naming viper anywhere.

## Decision

`api/config` defines a typed `PluginConfig` view and a `ReloadHandler` optional capability. `pkg/config` keeps viper as the implementation choice but never exports a viper type or names viper in any exported symbol. `cmd/server/main.go` drops its viper import. The string `viper` lives in exactly one place — `pkg/config/`'s implementation files — and the only library plugins import for configuration is `gocache/api/config`.

### `api/config` surface

```go
// PluginConfig is the read-only typed view of a single plugin's
// configuration subsection (e.g. plugins.config.snapshot.*).
// Keys are scoped — plugins read "file", not the full path.
type PluginConfig interface {
    GetString(key string) string
    GetInt(key string) int
    GetInt64(key string) int64
    GetBool(key string) bool
    GetDuration(key string) time.Duration
    GetStringSlice(key string) []string
    IsSet(key string) bool

    // SetDefault registers a fallback for key. Get* returns this value
    // when the operator hasn't set the key in YAML/env. Optional —
    // a plugin that doesn't call SetDefault for a key is declaring
    // the key required, and Build should return an error if Get*
    // comes back empty.
    SetDefault(key string, value any)
}

// ReloadHandler is the optional capability a Provider implements to
// react to runtime config-file reloads. The persistence layer
// (and parallel registries — AOF, replication) type-asserts on this
// after Build returns; a Provider that doesn't implement it simply
// skips reload, which is the right default for plugins with no
// runtime-tunable knobs.
type ReloadHandler interface {
    OnConfigReload(cfg PluginConfig)
}
```

### `api/persistence.SnapshotProvider.Build` gains a parameter

```go
type SnapshotProvider interface {
    Name() string
    Build(cfg apiconfig.PluginConfig) (Source, Snapshotter, error)
}
```

### `pkg/config` surface (library-agnostic names)

| Symbol | Purpose |
|---|---|
| `Load(flags) (*Config, error)` | Load returns only `*Config`; the previous `*viper.Viper` return is gone. |
| `Reload() (*Config, error)` | Re-parse config using the internal handle. No viper parameter. |
| `OnReload(fn func())` | Server-internal reload hook, no payload — the caller already has whatever state it needs in scope. |
| `OnPluginReload(name string, fn func(apiconfig.PluginConfig))` | Plugin-typed reload hook. Builds a fresh scoped view per fan-out. |
| `PluginConfigFor(name string) apiconfig.PluginConfig` | Returns a never-nil scoped view (`nopConfig{}` if `Load` hasn't run yet). |
| `ConfigFileUsed() string` | Replaces the previous `v.ConfigFileUsed()` call in `main.go`'s boot log. |

The internal `pluginView` adapter (viper-backed) and `nopConfig` zero-value implement `apiconfig.PluginConfig`. Plugins never see either type by name.

### Plugin shape

```go
package snapshot

import (
    apiconfig "gocache/api/config"
    apipersistence "gocache/api/persistence"
)

const (
    keyFile     = "file"
    defaultFile = "snapshot.dat"
)

func init() {
    apipersistence.RegisterSnapshotProvider(&provider{})
}

type provider struct {
    src  *Source
    snap *Snapshotter
}

func (provider) Name() string { return "snapshot" }

func (p *provider) Build(cfg apiconfig.PluginConfig) (apipersistence.Source, apipersistence.Snapshotter, error) {
    cfg.SetDefault(keyFile, defaultFile)
    file := cfg.GetString(keyFile)
    if file == "" {
        return nil, nil, fmt.Errorf("snapshot: %q is required", keyFile)
    }
    p.src = NewSource(file)
    p.snap = NewSnapshotter(file)
    return p.src, p.snap, nil
}

// OnConfigReload — optional capability, type-asserted by the server.
func (p *provider) OnConfigReload(cfg apiconfig.PluginConfig) {
    if newFile := cfg.GetString(keyFile); newFile != "" {
        p.src.SetFilename(newFile)
        p.snap.SetFilename(newFile)
    }
}
```

The plugin imports zero config-library code. The package name drops the `v1` prefix — the format version is internal to the plugin and shouldn't surface in directory or registration names.

### YAML layout — unchanged

Still `plugins.config.<name>.<key>`, e.g. `plugins.config.snapshot.file`. The `.config.` infix continues to separate IPC-plugin-manager knobs (`plugins.enabled`, `plugins.dir`) from individual embedded plugin configs. No YAML churn beyond the one-time `snapshot-v1` → `snapshot` rename.

### Defaults policy

Server-level config (`server.address`, `persistence.snapshot_interval`, etc.) keeps its defaults in `api/config.Default*` constants — unchanged. Plugin-level defaults are the plugin's own choice: a plugin that calls `cfg.SetDefault(key, value)` is declaring the key has a fallback; a plugin that doesn't is declaring the key required, and its `Build` returns an error if `Get*` comes back empty.

## Alternatives Considered

### Alternative 1: Keep viper exposed (status quo from ADR-0007)

- **Pros**: Already shipped. Zero new code. Plugins use viper directly with no abstraction.
- **Cons**: Plugin contract surface names a third-party library. Swapping config libraries (koanf, knadh, stdlib flag) becomes a breaking change for every plugin. The contract is the leak.
- **Why not**: The user's framing makes this a hard line: *"library names do not define functions, functionality is just config."* Naming the role, not the library, is the whole microkernel premise applied to config.

### Alternative 2: Pass a raw `map[string]any` to `Build`

- **Pros**: Maximally minimal — no interface methods, plugins parse the map themselves.
- **Cons**: Loses type safety. Every plugin re-implements duration parsing, type assertion, default handling. The "library-agnostic" goal is achieved but the cost is higher per plugin.
- **Why not**: A typed interface is the same contract surface as a map but with the type plumbing factored out once. Cheaper for every plugin author and no harder for the server to provide.

### Alternative 3: Plugin author defines a struct, server unmarshals into it

```go
type Config struct {
    File string `yaml:"file" default:"snapshot.dat"`
}
func (p *provider) Build(raw apiconfig.RawSection) (...) { var c Config; raw.Decode(&c); ... }
```

- **Pros**: Most idiomatic for Go. Defaults declared via struct tags. Type safety baked in.
- **Cons**: Hot reload semantics become awkward (re-decode into a new struct, diff field-by-field). Reflection-based decoding ties us to a specific decoder library — recreating the leak we set out to remove.
- **Why not**: Reintroduces the library-coupling problem one level lower. The `PluginConfig` getter interface is library-agnostic by construction.

### Alternative 4: Reload handler as a separate registration call

`pkg/config.OnPluginReload(name, fn)` invoked from the plugin's `init()` instead of being type-asserted on the Provider.

- **Pros**: Explicit subscription, no hidden type assertion magic.
- **Cons**: Forces every plugin to register two things — provider and reload handler. The provider already has the lifecycle hooks; making it the reload handler too is one fewer moving part.
- **Why not**: Type-asserting on the provider matches the existing optional-capability pattern (`LSNSeeder` in `api/persistence`). Consistent.

## Consequences

### Positive

- Plugins import `gocache/api/config` and `gocache/api/persistence` — nothing else from the server side. The `plugins/-imports-only-api/` rule (and its CI guard) becomes mechanically airtight.
- Swapping the underlying config library is a `pkg/config/` refactor with zero plugin impact.
- `cmd/server/main.go` no longer imports viper — server entrypoint is library-agnostic at the source level too.
- Test ergonomics improve: plugin tests inject a fake `PluginConfig` directly. No more `SetViperForTest` global swap.
- The "required vs optional key" pattern is explicit: `SetDefault` declares a fallback, absence of `SetDefault` declares the key required.
- Pattern generalizes: `RegisterAOFProvider`, `RegisterReplicationSink` get the same `Build(cfg PluginConfig)` shape and the same optional `ReloadHandler` capability. Adding a new plugin type doesn't grow `api/config`.
- Drops the `v1` impl-detail from the snapshot plugin's public name. Format versioning is internal — the public name is just `snapshot`.

### Negative

- One more layer between plugins and the underlying config store. Marginal indirection cost, paid in clarity.
- `PluginConfig` is wider than the ideal "small interface" — eight methods. Each is a single typed getter, but the interface has to enumerate every type plugins might need. Adding `GetFloat64` later is an additive change (every backend would need it), so the interface should grow only when a real plugin needs a new type.
- `SetDefault` mutates state across the boundary (plugin → server's config store). Acceptable because defaults are write-once at boot; alternative (a separate `Defaults()` method on the Provider) is more interface for marginal clean-up.

### Risks

- **Risk**: A plugin author types defensively, calls `SetDefault` for everything to avoid required-key errors. The "required vs optional" signal blurs. **Mitigation**: Document the convention in `api/config.PluginConfig` — required keys are deliberate; only call `SetDefault` for keys that genuinely have a sensible fallback.
- **Risk**: Test paths that don't run `pkg/config.Load` get `nopConfig{}` and silently see zero values. A plugin under test could `Build` successfully when its required key is missing because the test forgot to inject. **Mitigation**: `nopConfig.IsSet` returns false — plugin's required-key check (`if x == "" { return error }`) still fires. Tests that exercise required-key paths are responsible for injecting a real config view.
- **Risk**: Hot reload fan-out builds a fresh `pluginView` per plugin per reload — measurable cost if dozens of plugins are registered. **Mitigation**: Reloads are rare (operator-driven, fsnotify) and the view is a two-field struct. Cost is irrelevant in practice; revisit only if profiling says otherwise.
