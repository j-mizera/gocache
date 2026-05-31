---
title: Plugin SDK Guide
description: How to write GoCache plugins — IPC and embedded, interfaces, configuration, operations, logging, scopes
status: living
last_updated: 2026-05-31
related:
  - Plugins
  - GCPC
  - Persistence-Plugin-Guide
  - ADR-0019
  - ADR-0028
---

# Plugin SDK Guide

This guide covers writing both IPC and embedded plugins for GoCache. For persistence-specific details (Source, Sink, Snapshotter), see [persistence/README.md](persistence/README.md).

## Two Plugin Tiers

| | IPC Plugin | Embedded Plugin |
|---|---|---|
| Runs as | Separate process | Linked into server binary |
| Entry point | `pluginsdk.Run(ctx, plugin)` | `init()` registration |
| Isolation | Process boundary (crash-safe) | None (shares server process) |
| Transport | GCPC v1 over Unix socket | Direct function call |
| Config delivery | Server sends via `RegisterAckV1` | `PluginConfig` from Viper |
| When to use | Most plugins | Pre-config lifecycle, cache internals |

Both tiers import only from `gocache/api/*`. Never import `gocache/pkg/*` or `gocache/cmd/*`.

---

## IPC Plugins

IPC plugins run as separate processes. The server discovers plugin binaries in `plugins.dir`, fork/execs each one, and communicates over a Unix domain socket using the GCPC v1 protocol. The SDK (`sdk/pluginsdk`) handles the handshake and message loop.

### Minimal Plugin (lifecycle only)

```go
package main

import (
    "context"
    "os"
    "os/signal"
    "syscall"

    apilogger "gocache/api/logger"
    "gocache/sdk/pluginsdk"
)

type myPlugin struct {
    log *apilogger.Logger
}

func (p *myPlugin) Name() string    { return "myplugin" }
func (p *myPlugin) Version() string { return "1.0.0" }
func (p *myPlugin) Critical() bool  { return false }

func (p *myPlugin) OnHealthCheck(_ context.Context) error {
    return nil
}

func (p *myPlugin) OnShutdown(ctx context.Context) error {
    p.log.Info(ctx).Msg("shutting down")
    return nil
}

func main() {
    plog := apilogger.New(os.Stdout, "myplugin", "info")

    ctx, cancel := signal.NotifyContext(context.Background(),
        syscall.SIGTERM, syscall.SIGINT)
    defer cancel()

    if err := pluginsdk.Run(ctx, &myPlugin{log: plog}); err != nil {
        plog.ErrorNoCtx().Err(err).Msg("plugin error")
        os.Exit(1)
    }
}
```

### Plugin Interface (required)

Every IPC plugin implements `pluginsdk.Plugin`:

```go
type Plugin interface {
    Name() string
    Version() string
    Critical() bool
    OnHealthCheck(ctx context.Context) error
    OnShutdown(ctx context.Context) error
}
```

| Method | Purpose |
|--------|---------|
| `Name()` | Unique name, must match the binary filename |
| `Version()` | Semver string, reported in registration |
| `Critical()` | If `true`, server halts when this plugin crashes |
| `OnHealthCheck(ctx)` | Called periodically by the manager; return `nil` for healthy |
| `OnShutdown(ctx)` | Called on graceful shutdown; context carries the deadline |

### Optional Interfaces

Implement additional interfaces to opt into capabilities. A plugin can implement any combination.

#### CommandPlugin — handle RESP commands

```go
type CommandPlugin interface {
    Plugin
    Commands() []CommandDecl
    HandleCommand(ctx context.Context, cmd string, args []string,
        metadata map[string]string) *CommandResult
}
```

`Commands()` is called once during registration. Each `CommandDecl` describes one command:

```go
type CommandDecl struct {
    Name       string // e.g. "PUBLISH" or "QUERY"
    Namespaced bool   // true = REX namespace (PLUGIN:CMD), false = main
    MinArgs    int
    MaxArgs    int    // -1 = unlimited
    ReadOnly   bool   // hint: does not mutate state
}
```

`HandleCommand` is called concurrently from multiple goroutines — it must be goroutine-safe.

Return a `*CommandResult` with any of these types in `Value`:
- `string` — RESP simple string
- `int`, `int64` — RESP integer
- `float64` — RESP float (RESP3)
- `nil` — RESP null
- `error` — RESP error
- `[]any`, `[]string` — RESP array
- `map[string]string`, `map[string]any` — RESP map (RESP3)

```go
func (p *myPlugin) Commands() []pluginsdk.CommandDecl {
    return []pluginsdk.CommandDecl{
        {Name: "MYGET", Namespaced: true, MinArgs: 1, MaxArgs: 1, ReadOnly: true},
        {Name: "MYSET", Namespaced: true, MinArgs: 2, MaxArgs: 2},
    }
}

func (p *myPlugin) HandleCommand(ctx context.Context, cmd string,
    args []string, metadata map[string]string) *pluginsdk.CommandResult {
    switch cmd {
    case "MYGET":
        val := p.store[args[0]]
        return &pluginsdk.CommandResult{Value: val}
    case "MYSET":
        p.store[args[0]] = args[1]
        return &pluginsdk.CommandResult{Value: "OK"}
    default:
        return &pluginsdk.CommandResult{Value: fmt.Errorf("unknown command: %s", cmd)}
    }
}
```

#### HookPlugin — intercept core commands

```go
type HookPlugin interface {
    Plugin
    Hooks() []HookDecl
    HandleHook(ctx context.Context, req *HookRequest) *HookResponse
}
```

Hooks fire before (`HookPhasePre`) or after (`HookPhasePost`) core command execution. Patterns match command names exactly or via `"*"` wildcard.

```go
type HookDecl struct {
    Pattern string    // "SET", "GET", "*"
    Phase   HookPhase // HookPhasePre (1) or HookPhasePost (2)
}
```

`HookRequest` carries the command context:

```go
type HookRequest struct {
    Phase       HookPhase
    Command     string
    Args        []string
    ResultValue string            // post-hook only
    ResultError string            // post-hook only
    Context     map[string]string // server + shared context
    Metadata    map[string]string // REX metadata (bare keys)
}
```

Pre-hooks can deny commands:

```go
func (p *myPlugin) HandleHook(ctx context.Context,
    req *pluginsdk.HookRequest) *pluginsdk.HookResponse {
    if req.Phase == pluginsdk.HookPhasePre && req.Command == "DEL" {
        return &pluginsdk.HookResponse{
            Deny:       true,
            DenyReason: "deletes are disabled",
        }
    }
    return nil
}
```

Pre-hooks can also enrich the command context:

```go
return &pluginsdk.HookResponse{
    ContextValues: map[string]string{
        "shared.audit.user": currentUser,
    },
}
```

#### OperationHookPlugin — operation lifecycle

```go
type OperationHookPlugin interface {
    Plugin
    OperationHooks() []OperationHookDecl
    HandleOperationHook(ctx context.Context,
        req *OperationHookRequest) *OperationHookResponse
}
```

Operations are server-tracked units of work (commands, connections, snapshots, etc.). Operation hooks fire on start and complete.

```go
type OperationHookDecl struct {
    Type     string // operation type to match, "*" for all
    Priority int    // lower = fires first
}

type OperationHookRequest struct {
    OperationID       string
    OperationType     string
    ParentID          string
    Phase             string            // "start" or "complete"
    Context           map[string]string // visibility-filtered
    Replayed          bool
    ReplayStartUnixNs int64
}
```

On start, return context values to enrich the operation:

```go
func (p *myPlugin) HandleOperationHook(ctx context.Context,
    req *pluginsdk.OperationHookRequest) *pluginsdk.OperationHookResponse {
    if req.Phase == "start" {
        traceparent := p.tracer.Start(req.OperationID)
        return &pluginsdk.OperationHookResponse{
            ContextValues: map[string]string{
                "shared.traceparent": traceparent,
            },
        }
    }
    p.tracer.End(req.OperationID)
    return nil
}
```

#### EventPlugin — subscribe to server events

```go
type EventPlugin interface {
    Plugin
    EventTypes() []string
    HandleEvent(ctx context.Context, evt *gcpc.EventV1)
}
```

Events are fire-and-forget notifications. The current public observability event names are operation-first:

| Type | Fires when |
|------|------------|
| `operation.started` | A server-tracked operation begins. |
| `operation.completed` | A server-tracked operation completes or fails. |
| `runtime.logs` | The server log collector flushes a batch of runtime log records. |
| `replay.gap` | Event replay history was incomplete because retained history overflowed. |
| `connection.open` | Client connects. |
| `connection.close` | Client disconnects. |
| `server.start` | Server starts accepting traffic. |
| `server.shutdown` | Server begins shutdown. |
| `plugin.registered` | Plugin completes registration. |
| `plugin.crashed` | Plugin process crashes. |
| `plugin.restarted` | Plugin restarted after crash. |
| `config.reloaded` | Config file hot-reloaded. |
| `cache.eviction` | Key evicted by LRU. |

Hook-phase names such as `command.pre` and `command.post` describe synchronous reaction points, not the public event taxonomy. Runtime logs are delivered as batched `runtime.logs` diagnostics from `pkg/logcollector`; the old per-line `log.entry` event is removed. Event/log subscriptions should contribute to server-side interest masks so producers skip optional payload construction unless a plugin requested the exact signal and detail level.

```go
func (p *myPlugin) EventTypes() []string {
    return []string{"connection.open", "connection.close"}
}

func (p *myPlugin) HandleEvent(ctx context.Context, evt *gcpc.EventV1) {
    p.log.Info(ctx).Str("type", evt.Type).Msg("event received")
}
```

#### QueryPlugin — server introspection

```go
type QueryPlugin interface {
    Plugin
    SetSession(s *Session)
}
```

The SDK calls `SetSession` after registration, giving the plugin a `*Session` for querying the server:

```go
func (p *myPlugin) SetSession(s *pluginsdk.Session) {
    p.session = s
}

// Later, in a handler:
data, err := p.session.QueryServer(ctx, "health", nil)
// data["status"] = "ok"
```

Available query topics:

| Topic | Scope required | Returns |
|-------|----------------|---------|
| `health` | `server:query:health` | Server health status |
| `plugins` | `server:query:plugins` | Registered plugin list |
| `stats` | `server:query:stats` | Server statistics |
| `operation.start` | (internal query topic, not an event) | Start a server-tracked operation |
| `operation.complete` | (internal query topic, not an event) | Complete an operation |
| `operation.fail` | (internal query topic, not an event) | Fail an operation |

#### ConfigPlugin — react to config reloads

```go
type ConfigPlugin interface {
    Plugin
    OnConfigReload(cfg apiconfig.PluginConfig)
}
```

Called when the server's config file changes (hot reload). The `cfg` is a `RemoteConfig` backed by the server-delivered config map.

```go
func (p *myPlugin) OnConfigReload(cfg apiconfig.PluginConfig) {
    p.timeout = cfg.GetDuration("timeout")
    p.log.InfoNoCtx().Dur("timeout", p.timeout).Msg("config reloaded")
}
```

#### ScopePlugin — declare permissions

```go
type ScopePlugin interface {
    Plugin
    Scopes() []string
}
```

Without this interface, the plugin gets default scopes (`["read"]`). Declare what you need:

```go
func (p *myPlugin) Scopes() []string {
    return []string{
        "hook:post",
        "operation:hook",
        "events",
        "server:query:health",
    }
}
```

See [Scopes and Permissions](#scopes-and-permissions) for the full scope hierarchy.

### Plugin-Initiated Operations

IPC plugins can start server-tracked operations for async/background work. The server allocates an operation ID and tracks it.

```go
ctx, op, err := p.session.StartOperation(ctx, "custom_task")
if err != nil {
    return err
}

// op enriches the context — all logging within ctx now carries the operation_id
p.log.Info(ctx).Msg("starting background work")

// When done:
op.Complete()
// or on failure:
op.Fail("disk full")

// Add metadata:
op.Enrich("records_processed", "42")
```

`StartOperation` sends an internal server query (topic `operation.start`), which creates a tracked operation. `Complete()` and `Fail()` use internal query topics too; these are distinct from the public `operation.started` and `operation.completed` observability events consumed by instrumentation plugins.

---

## Embedded Plugins

Embedded plugins are compiled into the server binary via build tags. They run in-process and have direct access to Go APIs. Use them when you need to:

- Run before `config.Load` (e.g., lifecycle OTLP exporter from t=0)
- Access cache internals (e.g., persistence Source/Sink)
- Avoid IPC overhead for hot-path work

### EmbeddedPlugin Interface

```go
// api/embedded
type Plugin interface {
    Name() string
    BootInit(ctx context.Context) error
    ConfigLoaded(ctx context.Context, cfg *config.Config,
        pcfg config.PluginConfig) error
    ProcessShutdown(ctx context.Context) error
}
```

| Method | When called | Use for |
|--------|-------------|---------|
| `BootInit(ctx)` | Before config load | Early init (lifecycle OTLP exporter, env-var config) |
| `ConfigLoaded(ctx, cfg, pcfg)` | After config load | Config-dependent setup (crash dir scan) |
| `ProcessShutdown(ctx)` | Server shutting down (LIFO order) | Cleanup, flush |

The context carries an operation with `_plugin` and `_phase` enrichment — all logging within these methods is automatically correlated.

### Registration

```go
//go:build myplugin

package myplugin

import "gocache/api/embedded"

func init() {
    embedded.Register(&myPlugin{})      // non-critical (panic recovered)
    // or
    embedded.RegisterStrict(&myPlugin{}) // critical (error halts boot)
}
```

Add a tagless `doc.go` so the package compiles without the build tag:

```go
// Package myplugin provides ...
package myplugin
```

Build: `go build -tags myplugin ./cmd/server`

### Persistence Providers

Persistence plugins register via a separate registry in `api/persistence`:

```go
//go:build myplugin

package myplugin

import apipersistence "gocache/api/persistence"

func init() {
    apipersistence.RegisterProvider(&provider{})
}

type provider struct{}

func (*provider) Name() string { return "myplugin" }

func (*provider) Build(cfg apiconfig.PluginConfig,
    store apipersistence.CacheStore) (*apipersistence.Backend, error) {

    cfg.SetDefault("file", "default.dat")
    cfg.BindEnv("file", "GOCACHE_MYPLUGIN_FILE")
    file := cfg.GetString("file")

    return &apipersistence.Backend{
        Source:   newMySource(file),
        Sink:     newMySink(file),
        Commands: func(api apipersistence.PersistenceAPI) []apipersistence.Command {
            return []apipersistence.Command{
                {Name: "MYSAVE", Fn: handleSave(api), Spec: apicommand.Spec{Min: 0, Max: 0}},
            }
        },
        OnReload: &reloader{},
    }, nil
}
```

All `Backend` fields are optional. Implement only what you need. See [persistence/README.md](persistence/README.md) for the full Source/Sink/Snapshotter contract.

### Embedded Operations

Embedded plugins create operations directly via the `api/operations` package:

```go
import ops "gocache/api/operations"

func (p *myPlugin) doWork(ctx context.Context) error {
    ctx, op := ops.Begin(ctx, ops.TypeCommand)
    defer op.Complete()

    op.Enrich("task", "rebuild_index")
    p.log.Info(ctx).Msg("rebuilding index")
    // ... work ...
    return nil
}
```

`ops.Begin` creates an operation parented to any existing operation in the context. If no parent exists, it creates a root operation.

For background goroutines, propagate the operation without the parent context's cancellation:

```go
bgCtx := context.Background()
if op := ops.FromContext(ctx); op != nil {
    bgCtx = ops.WithContext(bgCtx, op)
}
go func() {
    // bgCtx carries the operation for log correlation
    // but won't be cancelled when the parent context expires
    p.log.Info(bgCtx).Msg("background task running")
}()
```

---

## Configuration

Both tiers use the same `api/config.PluginConfig` interface:

```go
type PluginConfig interface {
    GetString(key string) string
    GetInt(key string) int
    GetInt64(key string) int64
    GetBool(key string) bool
    GetDuration(key string) time.Duration
    GetStringSlice(key string) []string
    IsSet(key string) bool
    SetDefault(key string, value any)
    BindEnv(key string, envVars ...string)
    MergeFile(path string) error
}
```

### Embedded Plugin Config

Embedded plugins receive `PluginConfig` in `ConfigLoaded()` (backed by Viper, scoped to `plugins.<name>`). In `BootInit()`, config is not yet loaded — use `os.Getenv()` directly or defer setup to `ConfigLoaded()`.

```go
func (p *myPlugin) ConfigLoaded(ctx context.Context,
    cfg *config.Config, pcfg config.PluginConfig) error {

    pcfg.SetDefault("timeout_ms", 3000)
    pcfg.BindEnv("timeout_ms", "GOCACHE_MYPLUGIN_TIMEOUT_MS")
    p.timeout = pcfg.GetDuration("timeout_ms")
    return nil
}
```

### IPC Plugin Config

IPC plugins receive a `RemoteConfig` (implements `PluginConfig`) built from the config map the server sends in `RegisterAckV1`. The same `SetDefault`/`BindEnv` API works:

```go
func (p *myPlugin) OnConfigReload(cfg apiconfig.PluginConfig) {
    cfg.SetDefault("batch_size", 100)
    cfg.BindEnv("batch_size", "MYPLUGIN_BATCH_SIZE")
    p.batchSize = cfg.GetInt("batch_size")
}
```

### Priority Chain

Both tiers follow the same resolution order (highest wins):

1. Environment variable (via `BindEnv`)
2. Server/YAML value
3. Default (via `SetDefault`)

### YAML Config Shape (IPC plugins)

```yaml
plugins:
  enabled: true
  dir: ./plugins
  overrides:
    myplugin:
      scopes: ["read", "hook:post"]
      config:
        batch_size: "200"
        timeout: "5000"
```

The `config` map is delivered as `map[string]string` in `RegisterAckV1` and on hot reload.

---

## Logging

All plugins use the structured logger from `api/logger`:

```go
import apilogger "gocache/api/logger"

// Create in main():
plog := apilogger.New(os.Stdout, "myplugin", "info")
```

### Context-Aware Logging

When you have a `context.Context` carrying an operation, use the context-aware methods. The logger extracts the operation and injects `operation_id` and `_ctx` fields automatically:

```go
p.log.Info(ctx).Str("key", "value").Msg("processing request")
// Output: {"level":"info","source":"myplugin","operation_id":"op-42","key":"value","message":"processing request"}
```

### Without Context

Use `NoCtx` variants only when no context is available (e.g., in `main()` before `Run`):

```go
p.log.InfoNoCtx().Str("addr", port).Msg("server listening")
```

### Levels

`Trace`, `Debug`, `Info`, `Warn`, `Error`, `Fatal` — each has both `ctx` and `NoCtx` variants.

### Fluent API

```go
p.log.Error(ctx).
    Err(err).
    Str("file", path).
    Int("retries", 3).
    Dur("elapsed", elapsed).
    Msg("write failed")
```

### Package-Level Functions

`api/logger` also exposes package-level functions that delegate to a default logger (initialized by the server):

```go
import "gocache/api/logger"

logger.Info(ctx).Msg("hello")
logger.WarnNoCtx().Msg("no context available")
```

Embedded plugins typically use these. IPC plugins typically create their own `Logger` instance.

---

## Operation Context

Operations provide structured correlation across the entire request lifecycle. The logger, hooks, and traces all use operations for correlation.

### 4-Tier Context Security Model

Operation context keys follow a visibility model:

| Prefix | Visibility | Examples |
|--------|------------|---------|
| `_` (underscore) | Server-only; never sent to plugins | `_start_ns`, `_elapsed_ns`, `_command` |
| `<plugin>.` | Private to the named plugin | `instrumentation.traceparent` |
| `shared.` | Visible to all plugins | `shared.audit.user` |
| `.secret.` | Redacted in snapshots/logs | `.secret.token` |

Plugins receive a filtered view of the operation context — they see their own namespace, shared keys, and server keys the server explicitly exposes. They never see other plugins' private keys.

### Standard Context Keys

Defined in `api/command`:

| Key | Type | Description |
|-----|------|-------------|
| `_operation_id` | string | Unique operation identifier |
| `_start_ns` | uint64 | Operation start time (Unix nanoseconds) |
| `_elapsed_ns` | uint64 | Elapsed time (post-hook only) |
| `_command` | string | Command name |
| `_arg_count` | int | Number of arguments |
| `_result` | string | Command result (post-hook only) |
| `_error` | string | Error message (post-hook only) |
| `_trigger` | string | What triggered this operation |
| `_remote_addr` | string | Client remote address |
| `_plugin` | string | Plugin name |
| `shared.` | prefix | Shared context namespace |

### Operation Types

Defined in `api/operations`:

| Type | When |
|------|------|
| `command` | Processing a client command |
| `connection` | Client connection lifetime |
| `cleanup` | Background TTL cleanup |
| `snapshot` | Point-in-time snapshot |
| `startup` | Server boot |
| `shutdown` | Server shutdown |
| `config_reload` | Config hot-reload |
| `plugin_start` | Plugin lifecycle phase |
| `plugin_stop` | Plugin stopping |

---

## Scopes and Permissions

IPC plugins run in a permission sandbox. Declare required scopes via `ScopePlugin`, or accept the default (`["read"]`).

### Scope Hierarchy

```
admin > write > read

hook:pre          (independent)
hook:post         (independent)
operation:hook    (independent)
events            (independent)
keys:<pattern>    (key namespace restriction)

server:query > server:query:health
             > server:query:plugins
             > server:query:stats
```

`admin` implies `write` which implies `read`. The `hook:*`, `operation:hook`, and `events` scopes are independent — they don't inherit from `admin`.

### Scope Enforcement

| Capability | Required scope |
|------------|----------------|
| Handle read-only commands | `read` |
| Handle mutating commands | `write` |
| Handle admin commands | `admin` |
| Receive pre-hooks | `hook:pre` |
| Receive post-hooks | `hook:post` |
| Receive operation hooks | `operation:hook` |
| Receive events | `events` |
| Query server health | `server:query:health` |
| Query server plugins | `server:query:plugins` |
| Query server stats | `server:query:stats` |
| Key namespace restriction | `keys:user:*` |

### Server Config

Allowed scopes are configured per-plugin in YAML:

```yaml
plugins:
  overrides:
    myplugin:
      scopes: ["read", "write", "hook:post", "events"]
```

The server intersects requested scopes (from `ScopePlugin.Scopes()`) with allowed scopes (from config). Denied scopes are logged at startup; the plugin receives only granted scopes in `RegisterAckV1`.

Embedded plugins bypass scope checking — they run with full server privilege.

---

## The `main()` Pattern

Every IPC plugin follows this pattern:

```go
package main

import (
    "context"
    "os"
    "os/signal"
    "syscall"

    apilogger "gocache/api/logger"
    "gocache/sdk/pluginsdk"
)

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(),
        syscall.SIGTERM, syscall.SIGINT)
    defer cancel()

    plog := apilogger.New(os.Stdout, "myplugin", "info")

    plugin := &myPlugin{log: plog}

    if err := pluginsdk.Run(ctx, plugin); err != nil {
        plog.ErrorNoCtx().Err(err).Msg("plugin error")
        os.Exit(1)
    }
}
```

`pluginsdk.Run` does the following:
1. Reads `GOCACHE_PLUGIN_SOCK` from env (set by the plugin manager)
2. Connects to the Unix domain socket
3. Sends `RegisterV1` with plugin metadata, commands, hooks, scopes
4. Receives `RegisterAckV1` with granted scopes and config
5. Enters the message loop (commands, hooks, events, health checks, shutdown)
6. Returns when the context is cancelled or the server disconnects

### Signal Handling

Bind `SIGTERM` and `SIGINT` to the context before calling `Run`. The server sends `SIGTERM` during graceful shutdown; the SDK calls `OnShutdown(ctx)` with the deadline from the shutdown message.

### Binary Naming

The binary name must match `Name()`. Place it in the `plugins.dir` directory:

```
plugins/
  myplugin      # binary — Name() returns "myplugin"
  prometheus # binary — Name() returns "prometheus"
```

---

## Complete IPC Plugin Example

A plugin that implements commands, hooks, events, operations, config, and scopes:

```go
package main

import (
    "context"
    "fmt"
    "os"
    "os/signal"
    "sync/atomic"
    "syscall"

    apiconfig "gocache/api/config"
    gcpc "gocache/api/gcpc/v1"
    apilogger "gocache/api/logger"
    "gocache/sdk/pluginsdk"
)

type auditPlugin struct {
    log     *apilogger.Logger
    session *pluginsdk.Session
    count   atomic.Int64
}

// Plugin (required)
func (p *auditPlugin) Name() string    { return "audit" }
func (p *auditPlugin) Version() string { return "1.0.0" }
func (p *auditPlugin) Critical() bool  { return false }

func (p *auditPlugin) OnHealthCheck(_ context.Context) error { return nil }

func (p *auditPlugin) OnShutdown(ctx context.Context) error {
    p.log.Info(ctx).Int64("total_commands", p.count.Load()).Msg("shutting down")
    return nil
}

// ScopePlugin
func (p *auditPlugin) Scopes() []string {
    return []string{"hook:post", "events", "server:query:health"}
}

// HookPlugin
func (p *auditPlugin) Hooks() []pluginsdk.HookDecl {
    return []pluginsdk.HookDecl{
        {Pattern: "*", Phase: pluginsdk.HookPhasePost},
    }
}

func (p *auditPlugin) HandleHook(ctx context.Context,
    req *pluginsdk.HookRequest) *pluginsdk.HookResponse {
    p.count.Add(1)
    p.log.Debug(ctx).Str("cmd", req.Command).Msg("command executed")
    return nil
}

// EventPlugin
func (p *auditPlugin) EventTypes() []string {
    return []string{"connection.open", "connection.close"}
}

func (p *auditPlugin) HandleEvent(ctx context.Context, evt *gcpc.EventV1) {
    p.log.Info(ctx).Str("type", evt.Type).Msg("event")
}

// QueryPlugin
func (p *auditPlugin) SetSession(s *pluginsdk.Session) {
    p.session = s
}

// ConfigPlugin
func (p *auditPlugin) OnConfigReload(cfg apiconfig.PluginConfig) {
    p.log.InfoNoCtx().Msg("config reloaded")
}

func main() {
    ctx, cancel := signal.NotifyContext(context.Background(),
        syscall.SIGTERM, syscall.SIGINT)
    defer cancel()

    plog := apilogger.New(os.Stdout, "audit", "debug")

    if err := pluginsdk.Run(ctx, &auditPlugin{log: plog}); err != nil {
        plog.ErrorNoCtx().Err(err).Msg("plugin error")
        os.Exit(1)
    }
}
```

---

## Testing

### IPC Plugin Tests

Test handler logic directly — no need to stand up the full GCPC transport:

```go
func TestHandleCommand(t *testing.T) {
    p := &myPlugin{log: apilogger.New(os.Stdout, "test", "debug")}

    result := p.HandleCommand(context.Background(), "MYGET", []string{"key1"}, nil)
    if result.Value != expected {
        t.Errorf("got %v, want %v", result.Value, expected)
    }
}
```

### Embedded Plugin Tests

Use `api/config.NewMapConfig()` as a test stand-in for `PluginConfig`:

```go
func TestBuild(t *testing.T) {
    cfg := apiconfig.NewMapConfig()
    cfg.Values["file"] = "test.dat"

    store := apipersistence.NewMemoryStore()
    backend, err := provider.Build(cfg, store)
    if err != nil {
        t.Fatal(err)
    }
    // assert backend fields...
}
```

### Build Tags

Run embedded plugin tests with the appropriate build tag:

```bash
go test -tags myplugin -race ./plugins/myplugin/...
```

---

## See Also

- [Plugins overview](README.md) — Embedded vs IPC at a glance, build tags, scope system
- [Persistence plugin guide](persistence/README.md) — Source, Sink, Snapshotter, CacheStore contracts
- [GCPC v1 protocol](../gcpc/README.md) — Wire format specification
- [ADR-0008](../adr/0008-plugin-config-and-reload-contract.md) — Plugin config and reload contract
- [ADR-0018](../adr/0018-plugin-config-autonomy.md) — Plugin config autonomy (SetDefault/BindEnv)
- [ADR-0019](../adr/0019-unified-plugin-config-delivery.md) — Unified plugin config delivery
- [prometheus reference](prometheus/README.md) — Full-featured IPC plugin example
