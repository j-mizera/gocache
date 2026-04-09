# Solution Architecture

## Overview

GoCache is a Redis-compatible in-memory cache server built on a **microkernel architecture**. The core binary handles basic caching operations -- 5 data types, RESP protocol, memory management, persistence. All extended functionality (Pub/Sub, Kafka, geospatial, auth, metrics, replication) runs as plugins in separate OS processes.

**Thesis statement**: Safe extensibility and high performance are not in conflict -- process separation handles the common case, shared memory handles the exceptional case, and the hot path (core data types) is never touched by either.

## Design Principles

### Microkernel

The core question for every feature: **is this basic caching, or is it an extension?**

Core (in the binary):
- RESP protocol handling
- 5 basic data types and their operations
- Memory management, eviction, TTL
- Persistence (snapshots)
- Command dispatch, transactions
- Basic AUTH

Plugin (separate process):
- Pub/Sub, Kafka, Streams
- Geospatial, Bitmap, HyperLogLog
- Full-text search, time-series
- OAuth2/OIDC, ACL
- Prometheus/OpenTelemetry
- Replication, clustering

### Serial Dispatch

All cache mutations are serialized through a single command dispatch loop. Commands arrive via a buffered channel, the cache lock is acquired, the command executes, and the result is sent back on a per-command response channel.

This eliminates data races without fine-grained locking. The cache lock is held only during command execution. The bottleneck is the single dispatch thread for writes, but this matches the Redis model and simplifies correctness.

### Process Isolation

Plugins run as separate OS processes forked by the plugin manager. Communication happens over Unix domain sockets with Protobuf-encoded messages (GCPC protocol). A crashing plugin:

- Cannot corrupt server memory
- Is automatically restarted (non-critical) or triggers server shutdown (critical)
- Has its commands and hooks cleanly unregistered on disconnection

## Component Architecture

### Connection Handler

TCP listener with a dedicated handler per connection. Each connection maintains its own protocol reader/writer and tracks per-session state: authentication status, protocol version (RESP2/RESP3), transaction queue, and watched keys. Supports pipelining via buffered writes flushed when the client input buffer is drained.

### Command Evaluator

Central command dispatch hub. Maintains a registry mapping command names to handler functions. On command arrival:

1. Look up handler in the core command registry
2. If not found, fall through to the plugin router
3. Validate argument count against the command's spec (min/max)
4. If inside a transaction (MULTI), queue instead of executing
5. Run pre-hooks (critical hooks can deny the command)
6. Execute handler via the serial dispatch loop
7. Run post-hooks

### Dispatch Engine

Single-threaded command dispatch loop. Receives command closures on a buffered channel (capacity 100), acquires the cache lock, executes, and returns the result. Provides both fire-and-forget and request-response dispatch modes. Shutdown is coordinated via a stop signal channel.

### In-Memory Store

The cache stores entries in hash maps, one per data type:

| Type | Internal Representation |
|------|------------------------|
| String | Byte string |
| List | Dynamic array of strings |
| Hash | String-to-string map |
| Set | String set (map with empty values) |
| Sorted Set | Skip list + hash map (dual index for score and member lookups) |

Each entry tracks: value, value type, expiration timestamp (nanosecond precision), estimated size in bytes, and last access time.

**Memory management**: Per-entry overhead (128 bytes for map bucket, entry struct, LRU node) plus key/value sizes are summed. When the configured limit is exceeded, the eviction policy kicks in.

**Eviction**: LRU via a doubly-linked list. On memory breach, the least-recently-accessed key is evicted. Two modes: `lru` (default) and `noeviction` (reject writes with OOM error).

**TTL**: Dual strategy. Passive: expired entries are deleted on access. Active: a background worker sweeps all keys at a configurable interval (default 1 minute), deleting any that have expired.

### Plugin System

#### Transport Layer

Length-prefixed framing over Unix domain sockets. Each message is a 4-byte big-endian length header followed by a serialized Protobuf message. A write mutex ensures concurrent senders don't interleave frames.

#### GCPC Protocol

GoCache Plugin Communication protocol, versioned from day one (currently v1). All messages are wrapped in a typed envelope with a correlation ID for request/response matching.

| Message | Direction | Purpose |
|---------|-----------|---------|
| Register | Plugin -> Server | Announce name, version, commands, hooks, scopes |
| RegisterAck | Server -> Plugin | Accept/reject with granted scopes |
| HealthCheck | Server -> Plugin | Periodic liveness probe |
| HealthResponse | Plugin -> Server | Health status |
| Shutdown | Server -> Plugin | Graceful termination request with deadline |
| ShutdownAck | Plugin -> Server | Shutdown acknowledgement |
| CommandRequest | Server -> Plugin | Dispatch a client command to the plugin |
| CommandResponse | Plugin -> Server | Command result (recursive RESP-like value tree) |
| HookRequest | Server -> Plugin | Pre/post hook invocation |
| HookResponse | Plugin -> Server | Allow/deny decision (pre-hooks only) |

#### Lifecycle Manager

Manages plugin processes through a state machine: Loaded -> Starting -> Connected -> Registered -> Running -> Shutdown.

- **Discovery**: Scans the plugin directory for executables, applies YAML configuration overrides for criticality, priority, and scopes
- **Launch**: Fork/exec with the plugin socket path passed as an environment variable. Process groups are used for clean signal delivery.
- **Health monitoring**: Periodic health probes at a configurable interval. Critical plugin failure terminates the server. Non-critical failures trigger restart (up to a configurable limit).
- **Shutdown**: Sends a shutdown request with a deadline. Waits for acknowledgement. Force-kills (SIGKILL to process group) on timeout.

#### Command Router

Routes client commands to the appropriate plugin over multiplexed IPC. Two namespace modes:

- **Main namespace**: Redis-compatible command names (e.g., `PUBLISH`, `GEOADD`). Transparent to clients -- they don't know a plugin is handling the command.
- **REX namespace**: Plugin-specific commands with a `PLUGIN:CMD` prefix (e.g., `KAFKA:PRODUCE`). For commands that have no Redis equivalent.

Safety mechanisms:
- **Shadow guard**: Plugins cannot register commands that overlap with core commands
- **Duplicate guard**: No two plugins can claim the same command name
- **Atomic registration**: All commands from a plugin are registered together or none are (all-or-nothing validation before mutation)

IPC multiplexing: Each plugin connection tracks pending requests by correlation ID. Responses are dispatched to the correct caller via a concurrent map. Fire-and-forget mode is available for async hooks.

#### Hook System

Pre/post command interception with priority-based execution order:

- **Non-critical hooks**: Fire-and-forget -- dispatched asynchronously, server does not wait
- **Critical hooks**: Dispatched sequentially in priority order (lower number = higher priority), server waits for each response
- **Pre-hooks**: Can deny commands. If any critical pre-hook returns deny, the command is aborted and the client receives a DENIED error.
- **Post-hooks**: Receive the command result as context. Cannot deny.
- **Fail-open**: If a critical hook times out or errors, the server logs a warning and allows the command. Safety over availability.
- **Zero-cost path**: A fast check skips all hook logic when no hooks are registered, adding zero overhead to the hot path.

#### Permission / Scope System

Scope-based access control for plugins. Plugins declare requested scopes at registration; the server grants the intersection with the configuration-allowed set.

| Scope | Grants | Hierarchy |
|-------|--------|-----------|
| `read` | Cache read operations | Default (least privilege) |
| `write` | Cache mutations | Implies `read` |
| `admin` | Server-level operations | Implies `write` |
| `hook:pre` | Pre-hook registration | Independent |
| `hook:post` | Post-hook registration | Independent |
| `keys:<pattern>` | Key namespace restriction (glob) | Independent |

Enforcement points:
- **Registration time**: Requested scopes validated against config allowlist. Hooks silently dropped if the plugin lacks the corresponding hook scope.
- **Runtime**: Operation type and key patterns checked before forwarding commands to plugins via IPC.

Default scope when no configuration override exists: `["read"]` (least privilege).

### Persistence

Snapshot-based persistence using Go's gob encoding with a count header. A background worker saves snapshots at a configurable interval (default 5 minutes). On startup, the most recent snapshot is loaded if configured. A final snapshot is saved during graceful shutdown.

### Configuration

Layered configuration with the following precedence: **CLI flags > environment variables (`GOCACHE_*`) > config file (YAML) > defaults**.

Hot reload via filesystem notifications: changes to memory limits, eviction policy, snapshot interval, cleanup interval, and log level take effect without restart. Address and port changes require a restart.

## Concurrency Model

| Component | Strategy |
|-----------|----------|
| Connection handling | One goroutine per client connection |
| Command dispatch | Single goroutine, buffered command channel |
| Cache access | Mutex-protected, held only during command execution |
| Plugin routing | Read-write lock for route table, concurrent map for pending IPC responses |
| Hook dispatch | Read-write lock for registry, priority-sorted execution |
| Scope checks | Read-write lock, map per plugin |
| Background workers | Independent goroutines with stop signals |

## Data Flow

### Core Command (Hot Path)

```
Client -> TCP -> RESP Parse -> Command Evaluator -> Handler Lookup
    -> Pre-hooks (if any) -> Dispatch Loop -> Cache Lock
    -> Execute -> Cache Unlock -> Post-hooks (if any)
    -> RESP Serialize -> TCP -> Client
```

### Plugin Command

```
Client -> TCP -> RESP Parse -> Command Evaluator -> Handler Miss
    -> Plugin Router -> Route Lookup -> IPC Send
    -> Unix Socket -> Plugin Process -> Handle Command
    -> Plugin Process -> Unix Socket -> Correlation Match
    -> RESP Serialize -> TCP -> Client
```

### Hook Flow

```
Command Arrives -> Evaluator
    -> Hook Executor -> Match pre-hooks for command
        -> Non-critical: fire async (no wait)
        -> Critical: send + wait response (sequential by priority)
            -> If denied: return DENIED to client, skip execution
    -> Execute command
    -> Hook Executor -> Match post-hooks for command
        -> Non-critical: fire async (no wait)
        -> Critical: send + wait acknowledgement (sequential)
```

## Shutdown Sequence

```
Signal (SIGTERM/SIGINT)
    1. Unblock all waiting BLPOP/BRPOP clients
    2. Stop accepting connections, drain active connections (10s timeout)
    3. Shut down plugins (send request, wait for ack, force-kill stragglers)
    4. Stop background workers (snapshot, TTL cleanup)
    5. Save final snapshot to disk
    6. Stop the dispatch engine
```

## Design Diagrams

### Server

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [System HLD](design/component/components.puml) | Full high-level system design |
| Component | [Core Subsystems](design/component/components_core.puml) | Evaluator, engine, storage, workers |
| Component | [Configuration](design/component/components_config.puml) | Config loading, layering, hot reload |
| Component | [Memory & Eviction](design/component/components_memory_eviction.puml) | LRU eviction internals |
| Sequence | [Command Flow](design/sequence/sequence_command_flow.puml) | Core hot path with hooks and plugin fallback |
| Sequence | [Transactions](design/sequence/sequence_transaction.puml) | MULTI/EXEC/DISCARD flow |
| Sequence | [TTL Expiry](design/sequence/sequence_ttl_expiry.puml) | Dual strategy: passive + active sweep |
| Sequence | [Persistence](design/sequence/sequence_persistence.puml) | Snapshot save and load |
| Sequence | [Hot Reload](design/sequence/sequence_hot_reload.puml) | Config file change detection and application |
| Sequence | [Graceful Shutdown](design/sequence/sequence_graceful_shutdown.puml) | 6-step shutdown sequence |
| State | [Connection Lifecycle](design/state/state_connection.puml) | Client connection FSM (auth, hooks) |
| State | [Server Lifecycle](design/state/state_server.puml) | Server startup and shutdown FSM |

### GCPC Protocol

See [GCPC documentation](../gcpc/README.md) for protocol details and the following diagrams:

| Category | Diagram | Description |
|----------|---------|-------------|
| Component | [IPC Architecture](../gcpc/design/component/components_ipc.puml) | Server-plugin transport and framing |
| Component | [Plugin Internals](../gcpc/design/component/components_plugin.puml) | Plugin process internal structure |
| Component | [Core + Plugin Overview](../gcpc/design/component/components_core_plugins.puml) | Core-plugin relationship |
| Component | [Command Routing](../gcpc/design/component/components_command_routing.puml) | Main + REX namespace routing |
| Component | [Hook & Priority](../gcpc/design/component/components_hooks_priority.puml) | Hook registry and priority dispatch |
| Component | [Permission Scopes](../gcpc/design/component/components_permission_scopes.puml) | Scope model and enforcement |
| Sequence | [Plugin Registration](../gcpc/design/sequence/sequence_plugin_registration.puml) | Registration handshake |
| Sequence | [Command Dispatch](../gcpc/design/sequence/sequence_plugin_command_routing.puml) | Plugin command routing over IPC |
| Sequence | [Hook Flow](../gcpc/design/sequence/sequence_plugin_commands.puml) | Pre/post hook execution |
| Sequence | [Scope Registration](../gcpc/design/sequence/sequence_scope_registration.puml) | Scope negotiation |
| Sequence | [Scope Enforcement](../gcpc/design/sequence/sequence_scope_enforcement.puml) | Runtime scope checks |
| State | [Plugin Lifecycle](../gcpc/design/state/state_plugin_lifecycle.puml) | Plugin FSM |
| State | [Hook Execution](../gcpc/design/state/state_hook_execution.puml) | Hook dispatch FSM |
| State | [Scope Resolution](../gcpc/design/state/state_scope_resolution.puml) | Scope validation FSM |
