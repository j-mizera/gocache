---
title: ADR-0020 Client Push via GCPC
description: Generic mechanism for IPC plugins to push RESP data to client connections
status: accepted
date: 2026-05-26
deciders: [witherxse]
related:
  - 0006-builtin-vs-third-party-transport
  - 0008-plugin-config-and-reload-contract
  - 0022-modular-performance-budget
---

# ADR-0020: Client Push via GCPC

## Context

GCPC v1 is strictly request-response: the server sends a `CommandRequestV1`, the plugin replies with one `CommandResponseV1`. This works for commands where the server waits for a single answer (PUBLISH returning a subscriber count, QUERY returning data), but it cannot support push semantics where a plugin needs to send unsolicited data to specific client TCP connections.

Redis Pub/Sub is the motivating case: when a client calls SUBSCRIBE, the plugin must push `["subscribe", channel, count]` confirmation messages. When another client PUBLISHes, the plugin must fan out `["message", channel, data]` to every subscribed connection. Neither fits the one-request-one-response model.

This is not a Pub/Sub-specific problem. Any future plugin with push needs — WebSocket relay, streaming queries, replication — hits the same wall. The solution must be generic.

Constraints:
- `resp.Writer` is stack-scoped inside `handleConnection` (server.go:264) — not accessible outside that goroutine
- Each connection's write path must be serialized — concurrent writes to the same TCP connection produce corrupt RESP output
- The mechanism must work within GCPC v1 without breaking existing plugins

## Decision

We add three generic primitives to GCPC and the server:

1. **`ClientPushV1` message** (plugin → server): carries a `connection_id` and raw RESP `bytes`. The Manager's `readLoop` receives it and writes the bytes to the identified connection.

2. **`ConnectionRegistry`** (server component): maps connection IDs to `ConnHandle` objects. Each `ConnHandle` wraps a `resp.Writer` and a `sync.Mutex`. All writes to a connection — both normal command responses and plugin pushes — go through the handle's mutex, preventing interleaved output.

3. **`suppress_response` flag on `CommandResponseV1`**: when a plugin sets this to `true`, the server skips writing the normal RESP response for that command. The plugin has already sent all necessary data via `ClientPushV1`. This handles commands like SUBSCRIBE that send multiple push messages instead of a single response.

Connections are identified by `connOp.ID` (the connection operation ID), exposed to plugins as `_connection_id` in the context map.

### Performance budget follow-up

This ADR remains accepted for the generic push primitive. ADR-0022 adds the performance budget and optimization sequence for the current implementation: direct `ClientPushV1` writes are a correct first data path, but benchmark captures show the current Pub/Sub fanout path is outside the <=20% modular overhead budget for fanout0/fanout1. Future work should optimize batching/backpressure or add a specialized built-in data-plane before treating Pub/Sub performance as accepted.

## Alternatives Considered

### Alternative 1: Dedicated push socket per connection

Each client connection gets a second Unix socket shared with interested plugins. Plugins write RESP directly to this socket; the server splices it into the client's TCP stream.

- **Pros**: Zero-copy potential; no protobuf overhead for push data
- **Cons**: One extra socket per connection × number of push-interested plugins; complex lifecycle management (who creates, who closes); splice is Linux-specific
- **Why not**: The socket-per-connection overhead scales poorly and adds significant complexity for a problem that ClientPushV1 solves with a single message type.

### Alternative 2: Poll-based approach (plugin stores, client fetches)

Plugin buffers messages internally. Server polls the plugin periodically or the client sends a FETCH command to retrieve pending messages.

- **Pros**: No new GCPC message types; no ConnectionRegistry
- **Cons**: Adds latency proportional to poll interval; FETCH is not Redis-compatible; requires client-side changes; fundamentally changes the push model to pull
- **Why not**: Incompatible with Redis Pub/Sub semantics where messages arrive as unsolicited pushes.

### Alternative 3: Server-side message queue per connection

Server maintains an internal queue per connection. Plugin posts messages to the queue via GCPC. Server drains the queue into the connection's writer.

- **Pros**: Decouples push rate from connection write rate; natural backpressure
- **Cons**: Unbounded memory growth if subscribers are slow; queue management complexity (max size, eviction policy, per-connection or global limits); adds latency vs direct write
- **Why not**: Over-engineered for the common case. ClientPushV1 with direct write is simpler and matches Redis's behavior (slow subscribers get disconnected, not buffered). If backpressure becomes a problem, it can be added to the ConnectionRegistry later without protocol changes.

## Consequences

### Positive

- Any IPC plugin can push data to any client connection — Pub/Sub, streaming, notifications
- The server remains ignorant of specific plugin semantics — it just writes bytes where told
- Per-connection mutex serialization is the same model Redis uses in multi-threaded I/O mode
- `suppress_response` is additive (field 3 on CommandResponseV1) — existing plugins ignore it

### Negative

- Per-connection mutex adds a lock acquisition to every command response write, even for connections with no active push consumers
- Raw RESP encoding responsibility shifts to the plugin (must format valid RESP bytes)
- `ClientPushV1` is fire-and-forget — no acknowledgement if the connection is gone (plugin learns via `connection.close` events asynchronously)

### Risks

- **Risk**: Malicious or buggy plugin pushes corrupt RESP to a connection. **Mitigation**: The push data is raw bytes — the server trusts the plugin to send valid RESP. Scope checking (`write` scope required) limits which plugins can push. Future work could validate RESP framing in the push handler.
- **Risk**: Per-connection mutex contention under high push + command rate. **Mitigation**: Contention is per-connection, not global. A connection receiving 10k pushes/sec while also processing 10k commands/sec is an extreme case; benchmarking will quantify the real impact.
