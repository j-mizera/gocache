---
title: ADR-0037 Telemetry tmpfs Shared-Memory IPC Architecture
description: Replace UNIX-socket telemetry delivery with /dev/shm tmpfs files coordinated via socket; strip all context materialization from the drain worker
status: proposed
date: 2026-06-24
deciders: [witherxse]
related:
  - ADR-0034
  - ADR-0036
  - ADR-0029
  - ADR-0030
---

# ADR-0037: Telemetry tmpfs Shared-Memory IPC Architecture

## Context

ADR-0034 established zero-allocation telemetry storage: command goroutines submit compact binary `TelemetryRecord` entries to the slot tracker with no heap allocation on the record path. ADR-0036 established batch-level pipelined telemetry loss as bounded/counted/visible by design — when the drain worker cannot keep up under pipelined load, operations are skipped with a counter.

T-PARALLEL measurements (June 2026) showed a ~53% skip rate in pipelined mode. Root cause analysis traced this to the drain worker's materialization path being CPU-bound. For every completed operation, the drain worker (`operation_tracker_drain_worker.go:416-468`) performs:

1. **`copyOperationContext`** (L451-468): allocates a `map[string]string` by visiting the connection context version via callback — one string allocation per key and per value. Then iterates `operation.ContextOverlay` and copies those entries too.
2. **`foldContextUpdate`** (L471+): parses binary payload into Go strings via `string(payload[pos:pos+keyLen])` — two string allocations per context delta (key + value).
3. **Six materialize functions** (L529-912): each transforms the context map into GCPC `EventV1` protobuf messages, copying the `map[string]string` into protobuf `map<string,string>` fields — another full map allocation per event type.

The irony: `TelemetryRecord.PayloadBytes()` already stores context deltas as compact length-prefixed binary. The drain worker unparses this binary into Go strings, inserts them into a `map[string]string`, then the materialize functions re-marshal that map into protobuf `map<string,string>`. This binary → string → map → protobuf roundtrip is pure structural waste on the drain hot path, and it is the reason the drain worker cannot recycle slots fast enough under pipelined load.

Prior OpenCode sessions (cass-memory) already flagged this: session `ses_169dc4a57ffe` recommended "OTLP should not depend on legacy tracker. Validate whether this honors zero-allocation/no materialization on command hot path."

## Decision

We replace the GCPC `EventV1` telemetry delivery path (event bus → plugin bridge → router FIFO → UNIX socket) with a **/dev/shm tmpfs shared-memory file** for telemetry data, and **strip all context materialization from the drain worker**.

Concretely:

1. **Drain worker becomes a protobuf serializer.** For each completed operation, the worker serializes the entire operation — operation ID, initial context (the connection context version snapshot at operation start, as `repeated Tag`), all telemetry items, context deltas — as a single `TelemetryOperation` protobuf message and writes the serialized bytes to the per-plugin tmpfs file. The initial context is retrieved via `VisitConnectionContextVersion` and serialized directly as `Tag` key-value pairs into the protobuf buffer — no `map[string]string` allocation, no intermediate string conversion. The protobuf buffer is `sync.Pool`'d. Zero materialize function calls. Later phases switch to vtprotobuf for zero-reflection static codegen.

2. **Per-plugin tmpfs file** at `/dev/shm/gocache-telemetry-<plugin>`, 15M ceiling, containing sequential length-prefixed serialized `TelemetryOperation` protobuf messages — whole operations, not individual records. The server `mmap`s this file; the plugin reads it with standard `pread`. No per-operation record limit (the previous cap of 24 under ADR-0036 was necessary because the socket buffer was 200KB — tmpfs is effectively unlimited).

3. **No server-push notification.** The plugin self-drives via polling the tmpfs file. When the plugin's read threshold is reached (byte count, e.g. 64KB, or 5-minute timeout), it sends `TelemetryAck{consumed_offset}` to the server. The server compacts the file during the plugin's pause, then sends a confirmation. The plugin resumes polling. The socket carries only `TelemetryAck` (plugin→server) and ack confirmation (server→plugin).

4. **New `scope.ScopeTelemetry`** replaces the `events` scope for telemetry subscription. Non-telemetry events (connection lifecycle, plugin lifecycle, cache eviction, runtime logs) remain on the existing `events` scope and continue to use the event bus → socket path unchanged.

5. **Context handled as Tags, not maps.** The `TelemetryOperation` proto carries `initial_context: repeated Tag` (the connection context snapshot at OpStart) plus context deltas inside `telemetry_items` as `TelemetryItem` entries of kind ContextUpdate/ContextRemove, each containing `Tag{keyLen, keyBytes, valLen, valBytes}` pairs. The drain worker serializes both the initial context and the deltas into the protobuf buffer — no parsing, no `map[string]string` construction. The plugin applies deltas on top of the initial context during reconstruction.

6. **Plugin-side context reconstruction.** The instrumentation (or lifecycleotlp) plugin deserializes `TelemetryOperation` messages from tmpfs. For each operation, it seeds its context state machine from `initial_context` (the snapshot at OpStart), then applies `telemetry_items` deltas (ContextUpdate/ContextRemove) on top to build the final context map. The plugin converts reconstructed operations to OTLP traces/logs. The plugin has no concurrent client connections to serve — its full CPU budget is available for reconstruction.

## Alternatives Considered

### Alternative 1: Keep socket transport, optimize materialization

Reduce allocations in `copyOperationContext` and `foldContextUpdate` using `sync.Pool`'d maps and pre-sized string builders.

- **Pros**: No protocol change, no tmpfs dependency, smaller blast radius, incremental.
- **Cons**: The materialize functions are structurally wrong — the binary → string → map → protobuf roundtrip is inherent waste. Even with pooled maps, the string conversions and map iterations remain. The fundamental problem is that the drain worker does work it should not be doing.
- **Why not**: The problem is not the transport speed, it is the processing cost. Even a zero-latency socket does not help if the worker spends 80% of its time materializing context maps that will be re-serialized into protobuf on the other side.

### Alternative 2: Ring buffer in shared memory instead of sequential file

Use an `mmap`'d fixed-size struct array with head/tail pointers instead of a sequential length-prefixed file.

- **Pros**: Fixed size, no truncation needed, O(1) random access, no file I/O syscalls.
- **Cons**: Ring layout is language-specific (struct padding, alignment, endianness). The plugin would need to know ring dimensions, head/tail pointer semantics, and memory ordering rules. This violates GoCache's hard requirement that IPC plugins be fully language-agnostic (any language with file I/O + protobuf).
- **Why not**: Sequential file with length-prefixed records is universally readable by any language. The `pread` syscall on tmpfs is effectively a memory copy (no disk I/O), so the performance difference vs ring buffer is negligible.

### Alternative 3: mmap'd protobuf file with standard google.golang.org/protobuf

Use standard protobuf marshal/unmarshal on the mmap'd file as the serialization layer.

- **Pros**: Type-safe, reflection-based, ecosystem-standard, well-understood.
- **Cons**: Reflection-based marshal allocates — `Descriptor` objects, `proto.Message` reflection boxes, `interface{}` conversions. Standard protobuf's `Marshal`/`Unmarshal` is 2–5× slower than vtprotobuf and allocates on every call. The goal is zero allocation; standard protobuf cannot deliver that.
- **Why not**: vtprotobuf (Phase 4, tracked separately) generates static marshal/unmarshal code with zero reflection overhead. Standard protobuf is the status quo that produced the current allocation problem.

## Consequences

### Positive

- Zero server-side heap allocation per operation on the drain path (one string alloc for operation ID — amortizable via `sync.Pool` or interning).
- Drain worker becomes a protobuf serializer: serialize completed operation as `TelemetryOperation` protobuf → write serialized bytes to mmap'd file → done. CPU freed for faster slot recycling, directly addressing the ~53% pipelined skip rate.
- No socket backpressure for telemetry — the plugin reads tmpfs at its own pace, and the server never blocks waiting for plugin consumption.
- Plugin has its full CPU budget for context reconstruction (no concurrent client connections, no Redis protocol parsing).
- Telemetry loss approaches zero: tmpfs ceiling is 15M per plugin vs the socket buffer's 200KB. Overflow handling is simpler (truncate oldest with a counter).
- `scope.ScopeEvents` retains non-telemetry events unchanged — connection lifecycle, plugin lifecycle, cache eviction, runtime logs continue to work without modification.

### Negative

- Protocol break: new `scope.ScopeTelemetry`, new `TelemetryOperation` data format plus `TelemetryAck` GCPC coordination message. Existing plugins that subscribe to `events` for telemetry must migrate to `scope.ScopeTelemetry`.
- Plugin complexity increases: the instrumentation plugin must maintain a per-operation context state machine for delta reconstruction. Context state is lost on plugin crash (recoverable by re-reading unconsumed file data on restart).
- tmpfs space management: 15M ceiling per plugin requires overflow handling (truncate oldest unconsumed data, increment a drop counter, emit a gap notification).

### Risks

- **Risk**: tmpfs not available (containers with restricted `/dev/shm`, chroot jails, exotic platforms). **Mitigation**: graceful fallback to the existing event bus → socket path. Scope check at plugin registration: if `/dev/shm` is not writable, the server falls back to `EventV1` delivery and logs a warning.
- **Risk**: Plugin crash loses in-progress context state machine. **Mitigation**: the plugin re-reads unconsumed data from the last acked offset on restart. The server retains all data until `TelemetryAck` is received. No data loss on plugin restart — only context reconstruction state is rebuilt.
- **Risk**: Memory pressure from multiple plugin tmpfs files. **Mitigation**: 15M ceiling per plugin, file truncated on ack. Total bounded by plugin count × 15M. For 5 plugins, worst case is 75M of tmpfs — well within typical `/dev/shm` defaults (50% of RAM on Linux).
- **Risk**: Concurrent write/read race conditions. **Mitigation**: the server is the sole writer; the plugin is the sole reader. Plugin polling plus `TelemetryAck{consumed_offset}` (plugin→server only) lets the server compact only after the plugin reports consumed bytes. No locks needed — single-writer/single-reader with polling and ack coordination.

## Evolved Decisions

- **D5 (Polling coordination)**: Plugin self-drives via polling. Server writes to tmpfs, plugin reads at its own pace, sends `TelemetryAck{consumed_offset}` on threshold. Server compacts during plugin pause, sends ack confirmation, plugin resumes.
- **D6 (Context visibility)**: Telemetry context is public by design. baseContext private keys filtered at source (drain worker). Runtime ContextUpdate/Delete deltas always public.
- **D7 (Migration period)**: Everything public during experimental phase. Will rework when telemetry context store and execution context store are split.
- **D8 (io.MultiWriter fan-out)**: Serialize once, write to all registered plugin tmpfs files via io.MultiWriter. Per-plugin ack independent.
