---
title: Plugins
description: Embedded vs IPC plugin tiers, build-tag matrix, plugin-safe import surface, available plugins
status: living
last_updated: 2026-05-31
related:
  - Server
  - GCPC
  - Plugin-Prometheus
  - Plugin-Instrumentation
  - ADR-0028
---

# Plugins

GoCache extends a microkernel core with two kinds of plugin: **embedded** (compile-time-linked, in-process) and **IPC** (separate process over GCPC v1 protocol on a Unix domain socket). Both implement contracts in `gocache/api/*`. The choice is per-plugin and depends on what the plugin does.

## Embedded vs IPC at a glance

| Property | Embedded | IPC |
|---|---|---|
| Process model | Linked into the server binary | Separate process, fork/exec'd by the manager |
| Activation | Build tag (`-tags <name>`) | YAML `plugins.enabled: true` + binary in `plugins.dir` |
| Configuration | Env vars (no YAML at boot, since they run before `config.Load`) | YAML config block + GCPC `RegisterV1` |
| Isolation | None — a panic in the plugin can kill the server unless `embedded.invoke` recovers | Process boundary; a plugin crash does not kill the server (configurable via `failure_policy: halt_server` for the rare critical cases) |
| Hot-swap | No (compile time) | Yes (manager restarts on crash, up to `max_restarts`) |
| Scope sandbox | None — embedded plugins can do anything the server can | Permission/scope system (`read`, `write`, `admin`, `hook:pre`, `hook:post`, `keys:<pattern>`) |
| Latency overhead | None — direct function call | One IPC round-trip per call (~tens of microseconds) |
| Best for | Things that must run before `config.Load`, or that need direct access to cache internals (snapshot dumps, AOF mutation feed, crash dumps, lifecycle OTLP from t=0) | Anything with heavy dependencies (database drivers, network clients), anything that must isolate failure (metrics exporters, custom auth, rate limiting) |

Both modes use the **same `api/*` interfaces**. The difference is registration and transport — embedded plugins call `embedded.Register(p)` from `init()`, IPC plugins run `pluginsdk.Run(p)` and the manager handshakes them over Unix socket.

## The plugin import rule

Plugins under `plugins/` may import only `gocache/api/*`, `gocache/sdk/*`, `gocache/commons/*`, the Go standard library, and external dependencies. They may **not** import `gocache/pkg/*` (server internals) or `gocache/cmd/*` (entry points). This is enforced by `scripts/check-plugin-isolation.sh`, which runs in CI as part of the lint job.

The stable contract surface starts in `gocache/api/`; shared helpers that are intentionally plugin-safe live in `gocache/sdk/` and `gocache/commons/`:

```
api/
├── command/        Hook context constants, Result types, HookExecutor interface
├── config/         Config data structs (Config, ServerConfig, PersistenceConfig, MemoryConfig, ...)
├── context/        4-tier context security (FilterForPlugin, MergeFromPlugin, RedactSecrets)
├── crashdump/      Dump types + Write/Scan/Delete (used by the crashdump embedded plugin)
├── embedded/       Embedded plugin lifecycle interface + registry (BootInit, ConfigLoaded, ProcessShutdown)
├── events/         Event types + Emitter interface
├── gcpc/v1/        GCPC v1 Protobuf types + helpers (the IPC wire format)
├── logger/         Shared logger (server + plugins write JSON to stdout)
├── operations/     Operation struct, Type constants, ID generation
├── plugin/         PluginsConfig, PluginOverride, FailurePolicy* — pure data
├── scope/          Scope hierarchy + parsing
└── transport/      Conn (framed protobuf I/O) + Listener (Unix domain socket)
```

When a plugin needs a symbol that currently lives in `pkg/`, the right move is to extract the data/interface portion to `api/` and leave the implementation in `pkg/` (with type aliases for back-compat where needed). #52/#53 established this pattern.

## Build-tag matrix (embedded plugins)

Embedded plugins are gated by build tags so default builds carry zero cost. Each plugin's package contains a tagless `doc.go` (so blank imports always resolve) and a tag-gated `<name>.go` that holds the `init()` registration.

| Tag | Plugin | What it does | Configured via |
|---|---|---|---|
| `crashdump` | `plugins/crashdump` | Scans `<dir>/crashes/` on `ConfigLoaded` and emits crash events into the event bus | `GOCACHE_CRASHDUMP_DIR`, `GOCACHE_CRASHDUMP_DISABLED` |
| `lifecycleotlp` | `plugins/lifecycleotlp` | Exports lifecycle OTLP spans from `BootInit`, `ConfigLoaded`, and `ProcessShutdown`; scoped to pre-IPC startup/failure/shutdown visibility only | `GOCACHE_LIFECYCLE_OTLP_ENDPOINT`, `GOCACHE_LIFECYCLE_OTLP_SERVICE`, `GOCACHE_LIFECYCLE_OTLP_TIMEOUT_MS`, `GOCACHE_LIFECYCLE_OTLP_INSECURE`, `GOCACHE_LIFECYCLE_OTLP_DISABLED` |
| `pprof` | `cmd/server/pprof_on.go` | Boots a `net/http/pprof` endpoint on `0.0.0.0:6060` (overridable via `GOCACHE_PPROF_ADDR`); sets `BlockProfileRate` and `MutexProfileFraction` so contention shows up in profiles | `GOCACHE_PPROF_ADDR` |

Combine multiple tags via go's space-separated syntax: `-tags "crashdump lifecycleotlp"`. The Docker image accepts a `PLUGINS=` build arg that maps to space-separated tags; `PPROF=1` adds the `pprof` tag.

```bash
# Plain binary — no embedded plugins linked.
go build -o bin/gocache-server ./cmd/server

# With embedded plugins (crashdump + lifecycle OTLP from t=0).
go build -tags "crashdump lifecycleotlp" -o bin/gocache-server ./cmd/server

# Docker (production shape — multi-arch, signed, on every main push).
docker build --build-arg PLUGINS=crashdump,lifecycleotlp -t gocache:full .

# Docker with pprof port published.
docker build --build-arg PLUGINS=crashdump,lifecycleotlp --build-arg PPROF=1 -t gocache:profiling .
```

## IPC plugins

IPC plugins are separate executables in `plugins.dir`. The plugin manager (`pkg/plugin/manager`) discovers them at boot, fork/execs each one, hands it a `GOCACHE_PLUGIN_SOCK` env var pointing at a per-plugin Unix domain socket, then handshakes via GCPC v1 (`RegisterV1` → `RegisterAckV1`). The plugin's main loop reads envelopes (commands, hooks, queries, events, operation hooks), dispatches them to handlers, and writes responses.

The plugin author SDK (`sdk/pluginsdk/`) wraps the GCPC handshake and message loop. Authors implement one or more interfaces:

| Interface | Provides |
|---|---|
| `Plugin` (required) | `Name`, `Version`, `Critical`, `OnHealthCheck`, `OnShutdown` |
| `CommandPlugin` | Plugin commands (REX-namespaced or main-namespace) |
| `HookPlugin` | Pre/Post command hooks |
| `OperationHookPlugin` | Operation lifecycle hooks (start/complete) |
| `EventPlugin` | Subscription to server events |
| `QueryPlugin` | Server introspection (`server:query:health`, `server:query:plugins`, `server:query:stats`, `server:query:metrics.commands`) |
| `ScopePlugin` | Declares requested scopes |

Bundled IPC plugins:

| Plugin | What it does |
|---|---|
| `plugins/dummy` | Lifecycle-only test plugin |
| `plugins/prometheus` | Prometheus `/metrics`, `/healthz`, and `/readyz`; core command counters and histograms are pulled from the generic `server:query:metrics.commands` aggregate snapshot so metrics do not require one IPC event per command |
| [`plugins/instrumentation`](instrumentation/README.md) | Runtime OTLP traces and logs over IPC; subscribes to `operation.started`, `operation.completed`, `runtime.logs`, and `replay.gap`; metrics remain out of scope |

## Observability contract direction

ADR-0028 defines the operation-first observability contract. Plugins should treat `operation.started` and `operation.completed` as the canonical lifecycle events. Other event records are opt-in child facts attached to operations, and runtime logs use the batched `runtime.logs` event (`RuntimeLogBatchEventV1`) instead of the removed per-line `log.entry` event.

Event subscriptions contribute to server-side interest masks. Those masks let hot-path producers skip optional payload construction entirely when no plugin/exporter requested a signal or detail level. The Prometheus plugin uses the low-cardinality `server:query:metrics.commands` pull path for metrics, while the `instrumentation` plugin owns runtime OTLP traces/logs and consumes periodically flushed `runtime.logs` batches from the existing log collector. The runtime slice intentionally does not introduce a Unix-socket/yamux log transport; that remains a separate performance spike only if benchmark scope is approved later.

## Permissions and scopes

IPC plugins declare `requested_scopes` in `RegisterV1`. The server validates them against config-allowed scopes (or the default `["read"]`) and grants a subset back in `RegisterAckV1`. Scopes are hierarchical: `admin > write > read`; `hook:pre`/`hook:post`, `operation:hook`, and `events` are independent; `server:query` grants server-query topics; `keys:<pattern>` restricts access to a key namespace.

Hooks are silently dropped if the plugin lacks the matching `hook:pre`/`hook:post` scope. Operation hooks need `operation:hook`. Server queries follow the `server:query:*` hierarchy (e.g., `server:query` grants all topics).

Embedded plugins are not scope-checked — they run with full server privilege. Use them only when the trade-off is acceptable.

## See also

- [docs/plugins/sdk-guide.md](sdk-guide.md) — Full SDK guide: writing IPC and embedded plugins, interfaces, config, operations, logging.
- [docs/server/README.md](../server/README.md) — Server overview, build, run, configuration, env vars.
- [docs/server/design/component/](../server/design/component/) — Component diagrams (core, memory eviction, event bus ring).
- [docs/server/design/sequence/](../server/design/sequence/) — Sequence diagrams (command flow, persistence, transactions, graceful shutdown).
- [docs/gcpc/README.md](../gcpc/README.md) — GCPC v1 protocol specification.
- [docs/plugins/prometheus/README.md](prometheus/README.md) — Pull-based metrics IPC plugin.
- [docs/plugins/instrumentation/README.md](instrumentation/README.md) — Runtime OTLP traces/logs IPC plugin.
