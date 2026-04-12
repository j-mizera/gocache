# GCPC -- GoCache Plugin Communication Protocol

GCPC is the binary protocol used for communication between the GoCache server and its plugins. Plugins run as separate OS processes and connect to the server over Unix domain sockets. All messages are Protobuf-encoded, versioned, and multiplexed via correlation IDs.

## Protocol Version

Current version: **v1**

The protocol is versioned from day one. Every message envelope carries a version field. Incompatible changes will increment the version number; the server can support multiple versions simultaneously.

## Wire Format

```
[4-byte big-endian length] [serialized EnvelopeV1]
```

Each frame is a length-prefixed Protobuf message. The length header is a 4-byte unsigned integer in big-endian byte order, specifying the size of the serialized envelope that follows. A write mutex on each connection ensures concurrent senders don't interleave frames.

## Envelope

Every message is wrapped in an `EnvelopeV1`:

| Field | Type | Description |
|-------|------|-------------|
| version | uint32 | Protocol version (must be 1) |
| id | uint64 | Correlation ID for request/response pairing |
| payload | oneof | One of the message types below |

The correlation ID enables multiplexed dispatch -- multiple commands can be in-flight simultaneously on a single connection, with responses matched to requests by ID.

## Message Types

### Registration (field numbers 10-11)

| Message | Direction | Purpose |
|---------|-----------|---------|
| Register | Plugin -> Server | Plugin announces its name, version, criticality, commands, hooks, priority, and requested scopes |
| RegisterAck | Server -> Plugin | Server accepts or rejects with reason and granted scopes |

### Health (field numbers 20-21)

| Message | Direction | Purpose |
|---------|-----------|---------|
| HealthCheck | Server -> Plugin | Periodic liveness probe with timestamp |
| HealthResponse | Plugin -> Server | Status report (ok/not ok with status string) |

### Lifecycle (field numbers 30-31)

| Message | Direction | Purpose |
|---------|-----------|---------|
| Shutdown | Server -> Plugin | Graceful termination request with nanosecond deadline |
| ShutdownAck | Plugin -> Server | Acknowledgement before process exit |

### Commands (field numbers 40-41)

| Message | Direction | Purpose |
|---------|-----------|---------|
| CommandRequest | Server -> Plugin | Dispatch a client command (name, args, request ID) |
| CommandResponse | Plugin -> Server | Result as a recursive RESP-like value tree |

### Hooks (field numbers 50-51)

| Message | Direction | Purpose |
|---------|-----------|---------|
| HookRequest | Server -> Plugin | Pre/post hook invocation with command context |
| HookResponse | Plugin -> Server | Allow/deny decision (pre-hooks) or acknowledgement (post-hooks) |

## Registration Handshake

1. Plugin connects to the server's Unix domain socket (path provided via `GOCACHE_PLUGIN_SOCK` environment variable)
2. Plugin sends `Register` with its capabilities:
   - **name** and **version** -- identity
   - **critical** -- whether server should crash if this plugin fails
   - **commands** -- list of command declarations (name, namespace mode, arg spec, read-only hint)
   - **hooks** -- list of hook declarations (pattern, phase)
   - **priority** -- execution priority for hooks (lower = higher priority)
   - **requested_scopes** -- permissions the plugin needs
3. Server validates against configuration, checks for command shadow/duplicate conflicts, resolves scopes
4. Server sends `RegisterAck` with acceptance status and granted scopes
5. If rejected, plugin receives reason string and connection is closed

## Command Namespacing

Plugins can register commands in two namespace modes:

- **Main namespace** (`namespaced: false`): Commands appear as standard Redis commands. Clients use them transparently (e.g., `PUBLISH`, `GEOADD`). Cannot shadow any of the core commands.
- **REX namespace** (`namespaced: true`): Commands are prefixed with the plugin name and a colon (e.g., `KAFKA:PRODUCE`). Used for plugin-specific operations with no Redis equivalent.

## Command Results

Command responses use `ResultV1`, a recursive value tree that mirrors RESP types:

| Variant | Maps to |
|---------|---------|
| simple_string | RESP simple string (+OK) |
| error | RESP error (-ERR ...) |
| integer | RESP integer (:42) |
| bulk_string | RESP bulk string ($...) |
| is_null | RESP null |
| double_val | RESP3 double |
| array | RESP array (*...) -- recursive |
| map_val | RESP3 map -- recursive |

## Hook Protocol

Hooks allow plugins to intercept commands before or after execution.

**Pre-hooks**: Server sends `HookRequest` with phase=PRE, command name, and arguments. Plugin responds with `HookResponse`. If `deny=true`, the command is aborted and the client receives a DENIED error.

**Post-hooks**: Server sends `HookRequest` with phase=POST, command name, arguments, and the serialized result. For non-critical plugins, this is fire-and-forget (no response expected). Critical plugins send an acknowledgement.

**Execution order**: Hooks fire in priority order (lower number first). Non-critical hooks fire asynchronously. Critical hooks fire sequentially -- the server waits for each response before proceeding to the next.

## Hook Context

Each command carries a per-command context map (`map<string, string>`) that flows through the hook chain. Pre-hooks can write values into the context; post-hooks can read the accumulated result. The context is discarded after the command completes.

### Three namespaces

| Namespace | Prefix | Written by | Visible to |
|-----------|--------|------------|------------|
| Server | `_` | Server only | All plugins |
| Plugin-private | `<plugin>.` | Plugin (auto-prefixed) | Owning plugin only |
| Shared | `shared.` | Plugin (explicit) | All plugins |

### Server-injected keys

| Key | When | Description |
|-----|------|-------------|
| `_start_ns` | Before pre-hooks | Command start timestamp (nanoseconds since epoch) |
| `_elapsed_ns` | Before post-hooks | Command execution duration (nanoseconds) |

These constants are available in the `command` package as `command.StartNs` and `command.ElapsedNs`. Hook context helpers live alongside them: `command.NewHookCtx()`, `command.MergeHookCtx()`, and `command.FilterHookCtx()`.

### Namespace enforcement

When a pre-hook response includes `context_values`, the server auto-prefixes each key with `<plugin_name>.` unless the key already starts with `shared.`. When building a hook request for a specific plugin, the server filters the context to include only:

- `_*` keys (server context)
- `<target_plugin>.*` keys (the target plugin's own namespace)
- `shared.*` keys (cross-plugin shared)

This prevents plugins from reading each other's private state. To share data across plugins, use the `shared.` prefix explicitly.

### Wire format

`HookRequestV1` carries the filtered context in a `map<string, string> context` field (field 7). `HookResponseV1` carries plugin-written values in a `map<string, string> context_values` field (field 4).

## REX Metadata

REX (RESP EXtensions) lets clients attach per-command or connection-scoped key-value metadata that flows to plugins through the hook context. This enables per-request auth tokens, multi-tenancy, and distributed trace context propagation.

### Capability negotiation

Clients opt into REX by negotiating the version in `HELLO`:

```
HELLO 3 AUTH user pass SETNAME myclient REXV 1
```

When `REXV 1` is negotiated, the server recognizes `META` lines as metadata directives that accumulate into the next command's context. Without negotiation, `META` is treated as an unknown command and REX has zero overhead.

### Per-command metadata (stateless, preferred)

`META` lines precede a command and carry a single key-value pair each. Each line gets a RESP `+OK` response (RESP-compliant request/response). On the next non-META command, accumulated metadata is attached, flows through pre-hooks and post-hooks, and is discarded.

```
META traceparent 00-abc123-def456-01
META authorization Bearer eyJhbG...
GET mykey
```

Values may contain spaces -- in binary RESP mode they're carried as bulk strings; in inline mode, arguments after the key are joined with spaces.

### Connection-scoped defaults (sticky)

`REX.META` is a standalone command for setting connection-level defaults that merge with per-command metadata on every request:

| Subcommand | Description |
|------------|-------------|
| `REX.META SET <key> <value>` | Set a single key-value pair |
| `REX.META MSET <k1> <v1> [<k2> <v2> ...]` | Set multiple pairs in one call |
| `REX.META GET <key>` | Read a stored value |
| `REX.META DEL <key>` | Remove a key |
| `REX.META LIST` | Return all stored metadata |

Defaults are stored in a thread-safe `rex.Store` attached to `ClientContext.RexMeta` (nil until first use). Works regardless of `REXV` negotiation.

### Precedence

Per-command `META` values override `REX.META` connection defaults for that single command:

```
per-command META  >  REX.META connection defaults  >  (absent)
```

### Reserved keys

Keys starting with `_` or `shared.` are rejected at validation time -- those prefixes are reserved for server-injected and plugin-shared namespaces in the hook context.

### Plugin visibility

Merged metadata is injected into the hook context under the `shared.rex.` prefix:

```
shared.rex.auth.jwt      = "eyJhb..."
shared.rex.tenant.id     = "team-a"
shared.rex.traceparent   = "00-abc..."
```

All plugins see `shared.rex.*` keys through the existing `shared.` visibility rule in `command.FilterHookCtx()` -- no changes to the hook context filtering mechanism.

### Implementation

The logic lives in `pkg/rex/`:

- `rex.Store` -- thread-safe connection-scoped metadata store
- `rex.ParseMeta(args)` -- parses META command arguments into `(key, value)`
- `rex.InjectIntoHookCtx(hookCtx, store, cmdMeta)` -- merges defaults + per-command metadata into the hook context with the `shared.rex.` prefix
- `rex.ValidateKey(key)` -- enforces the reserved-prefix rules

The `REX.META` handler lives in `pkg/rex/handler/handler.go` and is registered by the evaluator alongside core RESP handlers.

## Scope Negotiation

Plugins declare requested scopes in the `Register` message. The server validates against the configuration-defined allowlist and returns the granted set in `RegisterAck`.

Available scopes: `read`, `write`, `admin` (hierarchical), `hook:pre`, `hook:post` (independent), `keys:<glob>` (key namespace restriction).

If a plugin requests scopes beyond what the configuration allows, registration is rejected. If a plugin does not request scopes, it receives the default set (`["read"]`).

Hooks declared by a plugin are silently filtered based on granted scopes -- a plugin without `hook:pre` scope will have its pre-hook declarations dropped during registration.

## Schema Definition

The full Protobuf schema is at `proto/gcpc/v1/gcpc.proto`.

## Design Diagrams

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [IPC Architecture](design/component/components_ipc.puml) | Server-plugin IPC transport and framing |
| Component | [Plugin Internals](design/component/components_plugin.puml) | Plugin process internal structure |
| Component | [Core + Plugin Overview](design/component/components_core_plugins.puml) | How core server and plugins relate |
| Component | [Command Routing](design/component/components_command_routing.puml) | Main + REX namespace routing |
| Component | [Hook & Priority System](design/component/components_hooks_priority.puml) | Hook registry, executor, priority dispatch |
| Component | [Permission Scopes](design/component/components_permission_scopes.puml) | Scope model, validation, enforcement |
| Component | [Hook Context](design/component/components_hook_context.puml) | Context namespacing, lifecycle, visibility rules |
| Sequence | [Plugin Registration](design/sequence/sequence_plugin_registration.puml) | Registration handshake flow |
| Sequence | [Command Routing](design/sequence/sequence_plugin_command_routing.puml) | Plugin command dispatch over IPC |
| Sequence | [Hook Flow](design/sequence/sequence_plugin_commands.puml) | Pre/post hook execution with context propagation |
| Sequence | [Hook Context Flow](design/sequence/sequence_hook_context.puml) | Context namespacing and filtering across multiple plugins |
| Sequence | [REX Metadata](design/sequence/sequence_rex_metadata.puml) | HELLO REXV negotiation, META accumulation, hook context injection |
| Sequence | [Scope Registration](design/sequence/sequence_scope_registration.puml) | Scope negotiation during registration |
| Sequence | [Scope Enforcement](design/sequence/sequence_scope_enforcement.puml) | Runtime scope checks |
| State | [Plugin Lifecycle](design/state/state_plugin_lifecycle.puml) | Plugin FSM: Loaded -> Running -> Shutdown |
| State | [Hook Execution](design/state/state_hook_execution.puml) | Hook dispatch state machine |
| State | [Scope Resolution](design/state/state_scope_resolution.puml) | Scope validation and granting FSM |
