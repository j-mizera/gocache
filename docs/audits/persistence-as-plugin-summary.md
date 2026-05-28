---
title: Persistence as plugin — extraction summary
description: Thesis anchor documenting the extraction of persistence from core into the plugin system, final interface surface, validation approach
status: living
last_updated: 2026-05-24
related:
  - Plugins
  - Server-Architecture
  - ADR-0001
  - ADR-0002
  - ADR-0003
  - ADR-0005
  - ADR-0016
  - ADR-0017
  - Audit-per-shard-arc-summary
---

# Persistence as plugin — extraction summary

This document is the thesis anchor for the persistence-as-plugin arc. It records what was extracted, why, the final contract surface, what shipped, and how it's validated.

## What was extracted and why

GoCache started with persistence hardwired into the core: a gob-encoded snapshot function called directly from the engine under the global cache lock. This violated the microkernel thesis — persistence is not basic caching. The extraction moved all persistence implementation into plugins behind generic interfaces, so:

1. **The core has zero knowledge of any specific persistence format** (gob, AOF binary, snapshot binary, or any future format). It provides generic extension points; plugins use them.
2. **A user who doesn't need persistence pays nothing** — no interfaces allocated, no goroutines spawned, no mutation overhead. The hot-path gate is an atomic load of a sink count.
3. **Multiple persistence strategies coexist** — an AOF sink and a snapshot writer can run simultaneously. The server doesn't limit registration count or assume "exactly one" of anything.
4. **Crash in a persistence plugin doesn't corrupt the cache** — embedded plugins run in-process but the coordinator quarantines failed sinks rather than aborting.

## Interface surface (final state)

All contracts live in `api/persistence/`. The server implements the coordinator in `pkg/persistence/` — plugins never import `pkg/`.

### Core types

| Type | Purpose |
|------|---------|
| `LSN` | Monotonic 64-bit log sequence number, allocated by coordinator |
| `BootMode` | Trichotomy: Initial, Snapshot, Replay (ADR-0002) |
| `FsyncPolicy` | Durability contract: Always, EverySec, No (ADR-0003) |
| `Mutation` | One durable write: LSN + Op + Key + Args (RESP bulk-string framing) |
| `SnapshotEntry` | One key-value tuple: Key + ValueType + Encoding + Value + Expiration |
| `ValueType` | Bytes, List, Hash, Set, SortedSet |
| `Encoding` | Native (Go types) or Packed (flat byte buffer) |

### Interfaces

| Interface | Role | Methods |
|-----------|------|---------|
| `Source` | Boot-time recovery | `Name`, `Boot` |
| `Sink` | Runtime mutation consumption | `Name`, `FsyncPolicy`, `Apply`, `Close` |
| `Snapshotter` | On-demand point-in-time dump | `Name`, `SaveSnapshot` |
| `CacheStore` | Coordinator's view of cache | `CaptureSnapshot`, `LoadEntry`, `Clear`, `ApplyMutation` |
| `Provider` | Plugin registration handle | `Name`, `Build` |
| `PersistenceAPI` | Coordinator surface for plugin commands | `Snapshot`, `LastSaveUnix` |

### Registration

Plugins call `RegisterProvider` from `init()` under a build-tag guard. The server calls `RegisteredProviders()` at startup and builds each one generically — it never knows what's behind the provider.

`Backend` (returned by `Provider.Build`) has five optional fields: Source, Sink, Snapshotter, Commands, OnReload. A plugin implements only what it needs.

## Built-in plugins shipped

### Snapshot plugin (`plugins/snapshot`)

- **Interfaces**: Source + Snapshotter
- **Format**: Binary format with magic header, version, and gob-encoded entries (ADR-0005)
- **Commands**: SAVE, BGSAVE, LASTSAVE
- **Build tag**: `snapshot` (not currently required — snapshot is part of the default build)

### AOF plugin (`plugins/aof`)

- **Interfaces**: Source + Sink
- **Format**: Binary varint-framed records with 10-byte header (magic "GOCAOF" + version), 8-byte LE LSN per record (ADR-0016)
- **Commands**: BGREWRITEAOF (concurrent rewrite prevented via TryLock)
- **Build tag**: `aof`
- **Boot recovery**: Forward scan of AOF file, torn records truncated at last-good offset (ADR-0017)
- **Replay**: Mutations re-executed via `CacheStore.ApplyMutation` — dispatches on Op to appropriate cache methods (ADR-0017)
- **Rewrite**: Captures live snapshot, synthesizes equivalent mutations (Hash→HSET, Set→SADD, List→RPUSH, Bytes→SET), writes compacted AOF, atomically replaces live file

## Zero-cost default

When no persistence plugin is compiled in or registered:

- `RegisteredProviders()` returns `nil`
- No coordinator, no sinks, no goroutines spawned
- `HasSinks()` returns `false` (atomic load of zero)
- Mutation emission gate (`if !coordinator.HasSinks() { return }`) short-circuits before any allocation
- No `Mutation` struct is built, no LSN is allocated, no channel send happens

The overhead of the persistence system when unused is one atomic load per cache write — effectively zero.

## Coordinator design

The coordinator (`pkg/persistence/coordinator.go`) orchestrates boot and runtime:

**Boot**: calls `Source.Boot()`, dispatches on `BootMode`:
- Initial → empty cache, LSN starts at 0
- Snapshot → `CacheStore.LoadEntry` for each entry, seed LSN from snapshot
- Replay → `CacheStore.ApplyMutation` for each mutation, track highest LSN

**Runtime**: spawns a `sinkChannel` goroutine per registered Sink. Each goroutine runs a group-commit flush loop:
- Accumulates mutations from a buffered channel (16K capacity)
- Flushes on: 1 ms elapsed, 64 KB accumulated, 80% buffer (high-water), or shutdown
- Calls `Sink.Apply(batch)` serially
- Quarantines on `ErrSinkFatal` (decrements active sink count)
- Non-blocking emission — if the channel is full, the mutation is dropped with a warning

**Snapshot**: `CacheStore.CaptureSnapshot()` → build iterator → `Snapshotter.SaveSnapshot(src)`

## Validation

### Test coverage

| Area | Tests | Method |
|------|-------|--------|
| AOF write + boot + replay | `TestIntegration_WriteBootReplayVerify` | Write mutations via Sink, boot from AOF, replay into real `*cache.Cache`, verify state |
| AOF torn record recovery | `TestTornRecord_Truncation` | Write partial record, boot, verify truncation at good offset |
| AOF concurrent rewrite | `TestBGREWRITEAOF_ConcurrentRejection` | Lock rewriting mutex, call BGREWRITEAOF, verify rejection |
| AOF codec round-trip | `TestCodecRoundTrip` | Encode → decode for all operation types |
| ApplyMutation dispatch | `TestApplyMutation/*` | Table-driven: SET, DEL, HSET, HDEL, SADD, SREM, LPUSH, RPUSH, LSET, SETNX, GETSET, GETDEL, INCRBYFLOAT, EXPIRE, PEXPIRE, SPOP |
| Provider registry | `TestRegisterProvider*` | Registration, duplicate panic, concurrent safety |
| Snapshot entry round-trip | Snapshot plugin tests | Write → load → verify for all value types |

### Build verification

```bash
go build -tags "aof crashdump lifecycleotlp" ./...
go test -tags "aof crashdump lifecycleotlp" -race ./...
go vet ./...
staticcheck ./...
```

### Plugin isolation enforcement

`scripts/check-plugin-isolation.sh` runs in CI and verifies that `plugins/` packages import only `gocache/api/*` — never `gocache/pkg/*` in production code. Test files (`_test.go`) are exempt.

## Architecture validation against thesis

| Thesis claim | How persistence validates it |
|-------------|------------------------------|
| Safe extensibility and high performance are not in conflict | Persistence plugins run behind generic interfaces; the hot-path gate is one atomic load; quarantine prevents plugin failure from affecting cache |
| Process separation handles the common case | IPC plugins (future Kafka, Postgres) use GCPC protocol over Unix socket |
| Shared memory handles the exceptional case | Embedded plugins (AOF, snapshot) share cache memory for zero-copy snapshot capture |
| The hot path is never touched by plugins | Mutation emission is gated by `HasSinks()` — no sinks = no overhead; with sinks, the overhead is one atomic increment (LSN) + one non-blocking channel send per write |

## ADR trail

| ADR | Decision |
|-----|----------|
| 0001 | Persistence is pluggable, keyed by log + snapshot + LSN |
| 0002 | Source/Sink split with BootMode trichotomy |
| 0003 | Group commit + per-Sink fsync policy |
| 0005 | Snapshot binary wire format |
| 0006 | Built-in (embedded) vs third-party (IPC) transport choice |
| 0008 | Plugin config is library-agnostic (no Viper leak) |
| 0016 | AOF binary wire format (varint-framed, LE LSN) |
| 0017 | Mutation replay execution path (ApplyMutation dispatch) |
