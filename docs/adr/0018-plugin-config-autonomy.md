---
title: ADR-0018 Plugin config autonomy — BindEnv and MergeFile
description: Extend PluginConfig so plugins can declare short env var names and load their own config files
status: proposed
date: 2026-05-24
deciders: [witherxse]
related:
  - ADR-0008
  - Plugins
---

# ADR-0018: Plugin config autonomy — BindEnv and MergeFile

## Context

ADR-0008 established a library-agnostic `PluginConfig` interface backed by viper's `plugins.config.<name>.*` namespace. Plugins read scoped keys (`"file"`) that resolve to the full path (`plugins.config.aof.file`) inside viper. This works for YAML config but has two gaps:

1. **Env var ergonomics.** Viper's `AutomaticEnv` maps keys to env vars by uppercasing and replacing dots with underscores: `plugins.config.aof.file` → `GOCACHE_PLUGINS_CONFIG_AOF_FILE`. Operators expect `GOCACHE_AOF_FILE`. Plugins have no way to declare shorter aliases.

2. **No plugin-owned config file.** A plugin that ships its own `aof.yaml` cannot load it through `PluginConfig`. The only config sources are the server's YAML file + env vars. Operators managing many plugins must edit the central config file for every plugin-specific knob.

Lifecycle plugins (lifecycleotlp, crashdump) use raw `os.Getenv()` in `BootInit` because they run before `config.Load()`. This is a timing constraint inherent to the embedded plugin lifecycle, not a design flaw — they need configuration before the config system exists.

## Decision

We extend `PluginConfig` with two methods:

```go
BindEnv(key string, envVars ...string)
MergeFile(path string) error
```

`BindEnv` registers short env var aliases for a plugin key. The long-form name (`GOCACHE_PLUGINS_CONFIG_AOF_FILE`) continues to work via `AutomaticEnv`. Plugins call `cfg.BindEnv("file", "GOCACHE_AOF_FILE")` in `Build` before reading keys.

`MergeFile` reads a plugin-owned config file (YAML, JSON, or TOML — format auto-detected) and merges its keys under the plugin's namespace as defaults. Missing file is silently ignored; parse error is returned.

Merge priority (highest to lowest):
1. CLI flags
2. Env vars (BindEnv short names + AutomaticEnv long names)
3. Server config file (`gocache.yaml` under `plugins.config.<name>.*`)
4. Plugin config file (flat keys merged under prefix)
5. `SetDefault` values

We also consolidate the three duplicated `PluginConfig` test implementations into a single exported `MapConfig` in `api/config/`.

## Alternatives Considered

### Alternative 1: Plugin-level viper instance

Give each plugin its own viper with its own env prefix (`GOCACHE_AOF_*`), config file, and defaults.

- **Pros**: Full isolation, plugin controls everything.
- **Cons**: Fragments the config space — operators can't put all plugin config in one YAML file. Duplicates env-prefix wiring in every plugin. Breaks the single-viper reload model (fsnotify watches one file, not N).
- **Why not**: The centralized config file is a feature, not a limitation. Operators want one YAML with sections, not N files to manage. Adding a per-plugin file as an optional merge source preserves both.

### Alternative 2: Config struct with struct tags

Plugin declares a Go struct with `yaml:"..."` tags. Server decodes the plugin's config section into the struct automatically.

- **Pros**: Type-safe, no string key lookups.
- **Cons**: Rejected by ADR-0008 — the server would need to know the plugin's struct type at compile time, or use reflection to decode into an `any`. Either approach couples the server to plugin internals. The interface-based approach keeps the server generic.
- **Why not**: Violates plugin isolation. The server must remain ignorant of plugin-specific types.

### Alternative 3: Fully defer lifecycle plugin config to ConfigLoaded

Originally considered impossible because `BootInit` runs before `config.Load()`. Revisited: crashdump's `BootInit` only stores config — all work happens in `ConfigLoaded`. So config reading can move entirely to `ConfigLoaded` using `PluginConfigFor`. lifecycleotlp's `BootInit` genuinely creates the exporter (timing constraint is real), so it keeps env-var bootstrap with `PluginConfig` registration in `ConfigLoaded` for consistency.

- **Adopted for crashdump**: full migration to PluginConfig in ConfigLoaded.
- **Adopted for lifecycleotlp**: BootInit keeps `applyEnv()`; ConfigLoaded registers BindEnv + SetDefault so the config schema is documented and YAML-discoverable.

## Consequences

### Positive

- Operators use ergonomic env var names: `GOCACHE_AOF_FILE` instead of `GOCACHE_PLUGINS_CONFIG_AOF_FILE`.
- Plugins can ship their own config file as a self-contained default.
- Test helper consolidation eliminates ~80 lines of duplicated `mapConfig` implementations.
- Embedded plugins (crashdump, lifecycleotlp) use the same config pattern as persistence plugins — `SetDefault` + `BindEnv` + `Get*` via `PluginConfigFor`.
- `api/config.PluginConfigFor` gives all plugins a single entry point to their scoped config, regardless of plugin type.

### Negative

- `PluginConfig` interface grows by two methods. Every implementation (pluginView, nopConfig, test helpers) must add them.
- `api/config` gains mutable package-level state (`pluginConfigProvider`). Acceptable: set once by `pkg/config` during load, read-only thereafter.

### Risks

- **Risk**: `BindEnv` short-name collision — two plugins bind `GOCACHE_FILE`. **Mitigation**: Convention (use `GOCACHE_<PLUGIN>_<KEY>`). Document in plugin author guide. No runtime enforcement needed — operator error.
- **Risk**: `MergeFile` called after `Build` returns could introduce ordering-dependent behavior. **Mitigation**: Document that `MergeFile` is intended for use in `Build` only.
