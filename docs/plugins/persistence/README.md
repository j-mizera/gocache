---
title: Persistence Plugin Author Guide
description: How to write an embedded persistence plugin for gocache — interfaces, boot modes, fsync, encoding, testing
status: living
last_updated: 2026-05-24
related:
  - Plugins
  - Server-Architecture
  - ADR-0001
  - ADR-0002
  - ADR-0003
  - ADR-0005
  - ADR-0006
  - ADR-0008
  - ADR-0016
  - ADR-0017
---

# Persistence Plugin Author Guide

This guide covers everything you need to write an embedded persistence plugin for gocache. The persistence contract is defined in `api/persistence/` and documented across ADRs 0001–0003, 0005–0006, 0008, 0016–0017.

## Overview

Persistence plugins participate in two distinct lifecycle phases:

1. **Boot** — recover cache state from durable storage (Source)
2. **Runtime** — consume mutations as they happen (Sink), and optionally write point-in-time snapshots (Snapshotter)

Each phase has its own interface. A plugin implements only what it needs — no stub methods required.

## Interfaces

All interfaces live in `gocache/api/persistence`. Plugins import only `gocache/api/*` — never `gocache/pkg/*`.

### Source (boot-side recovery)

```go
type Source interface {
    Name() string
    Boot(ctx context.Context) (BootResult, error)
}
```

Boot is called exactly once per server lifetime, before client traffic starts. Return a `BootResult` with one of three modes:

| Mode | When | BootResult fields |
|------|------|-------------------|
| `BootModeInitial` | Nothing to recover (missing/empty file, first run) | None |
| `BootModeSnapshot` | Point-in-time snapshot available | `Snapshot` iterator + `LSN` |
| `BootModeReplay` | Mutation log available | `Replay` iterator |

**Iterators** are forward-only, single-pass. Yield entries/mutations until `io.EOF`, then the coordinator closes the iterator. On unrecoverable error, return it — boot errors abort server startup.

### Sink (runtime mutation feed)

```go
type Sink interface {
    Name() string
    FsyncPolicy() FsyncPolicy
    Apply(ctx context.Context, batch []Mutation) error
    Close(ctx context.Context) error
}
```

The coordinator group-commits mutations (1 ms or 64 KB trigger, ADR-0003) before calling `Apply`. Each `Apply` receives a non-empty batch in LSN order. The coordinator serializes Apply calls per Sink — no concurrent Apply on the same instance.

**Error handling:**
- Return `nil` on success.
- Transient errors (disk full, network timeout) are retried on the next batch.
- Fatal errors should wrap `ErrSinkFatal` — the coordinator quarantines the sink and stops sending.

### Snapshotter (on-demand dump)

```go
type Snapshotter interface {
    Name() string
    SaveSnapshot(ctx context.Context, src SnapshotSource) error
}
```

Called when the server needs a point-in-time dump (SAVE, BGSAVE, periodic, shutdown). The coordinator builds `src` from the live cache — the snapshotter iterates `src.Next()` and writes entries to its format. Atomicity is the snapshotter's responsibility (write temp file → fsync → rename).

Optional: implement `LSNSeeder` to receive the current LSN before `SaveSnapshot`:

```go
type LSNSeeder interface {
    SetLSN(lsn LSN)
}
```

### CacheStore (coordinator's view of cache)

Plugins don't implement this — the server's `*cache.Cache` does. You interact with it indirectly:

- **Boot replay**: the coordinator calls `CacheStore.ApplyMutation()` for each mutation your ReplayIterator yields.
- **Snapshot source**: the coordinator calls `CacheStore.CaptureSnapshot()` and feeds entries to your Snapshotter.

## Provider Registration

Embedded plugins register via `init()` using a build-tag guard:

```go
//go:build myplugin

package myplugin

import (
    "gocache/api/config"
    apipersistence "gocache/api/persistence"
)

func init() {
    apipersistence.RegisterProvider(&myProvider{})
}

type myProvider struct{}

func (*myProvider) Name() string { return "myplugin" }

func (*myProvider) Build(cfg config.PluginConfig, store apipersistence.CacheStore) (*apipersistence.Backend, error) {
    // Read config from cfg map, initialize resources
    return &apipersistence.Backend{
        Source:      newMySource(cfg),
        Sink:        newMySink(cfg),
        Snapshotter: nil, // optional
        Commands:    func(api apipersistence.PersistenceAPI) []apipersistence.Command {
            return []apipersistence.Command{
                {Name: "MYCOMMAND", Fn: handleMyCommand, Spec: mySpec},
            }
        },
        OnReload: func(cfg config.PluginConfig) {
            // Handle hot reload
        },
    }, nil
}
```

The server discovers all registered providers via `RegisteredProviders()` and builds them generically — it has zero knowledge of specific plugin implementations.

## Backend Fields

All `Backend` fields are optional:

| Field | Purpose | When to use |
|-------|---------|-------------|
| `Source` | Boot-time recovery | Plugin has on-disk state to recover |
| `Sink` | Runtime mutation consumption | Plugin persists ongoing writes |
| `Snapshotter` | Point-in-time snapshot writing | Plugin supports SAVE/BGSAVE-style dumps |
| `Commands` | Plugin-registered RESP commands | Plugin exposes user-facing commands |
| `OnReload` | Config hot-reload handler | Plugin has runtime-tunable knobs |

A pure archival Sink (e.g. Kafka writer) omits Source and Snapshotter. A one-shot boot loader (e.g. Postgres import) omits Sink. Implement exactly what you need.

## FsyncPolicy

Every Sink declares a durability policy via `FsyncPolicy()`:

| Policy | Behavior | Durability | Latency |
|--------|----------|------------|---------|
| `FsyncAlways` | fsync after every Apply batch | Highest — no data loss | Highest |
| `FsyncEverySec` | Background goroutine syncs ~1/s | ~1 s max loss on crash | Low |
| `FsyncNo` | Rely on OS page cache | ~30 s on Linux defaults | Lowest |

The Sink owns its own fsync implementation. The coordinator does not call fsync on behalf of Sinks.

## Mutation Format

```go
type Mutation struct {
    LSN  LSN      // Monotonic, allocated by coordinator
    Op   string   // Command name, uppercased: "SET", "HSET", "DEL", ...
    Key  string   // Primary key (first key for multi-key commands)
    Args [][]byte // RESP bulk-string args (write as-is, no re-encoding needed)
}
```

LSNs are allocated atomically by the coordinator. Your Sink sees mutations in LSN order within each batch. Across batches, LSN ordering is guaranteed for a single Sink.

## SnapshotEntry Format

```go
type SnapshotEntry struct {
    Key        string
    ValueType  ValueType   // Bytes, List, Hash, Set, SortedSet
    Encoding   Encoding    // Native (Go types) or Packed (byte buffer)
    Value      any         // Concrete type depends on ValueType + Encoding
    Expiration int64       // Unix nanoseconds; 0 = no expiry
}
```

Value type mapping for `EncodingNative`:

| ValueType | Go type |
|-----------|---------|
| `ValueTypeBytes` | `string` or `[]byte` |
| `ValueTypeList` | `[]string` |
| `ValueTypeHash` | `map[string]string` |
| `ValueTypeSet` | `map[string]struct{}` |
| `ValueTypeSortedSet` | `map[string]float64` |

For `EncodingPacked`, Value is always `[]byte` — the raw packed buffer.

## Build Tags and Activation

Embedded plugins are gated by build tags:

```go
//go:build myplugin
```

Add a tagless `doc.go` in your plugin package so blank imports always resolve:

```go
// Package myplugin implements a persistence plugin for ...
package myplugin
```

Build with your tag: `go build -tags myplugin ./cmd/server`

## Plugin Commands

Plugins register RESP commands via the `Commands` function in `Backend`. Commands receive a `PersistenceAPI` handle for coordinator interaction (trigger snapshots, query state):

```go
func(api apipersistence.PersistenceAPI) []apipersistence.Command {
    return []apipersistence.Command{
        {
            Name: "MYBGSAVE",
            Fn: func(ctx context.Context, args []string) (any, error) {
                go func() {
                    if err := api.Snapshot(ctx); err != nil {
                        // log error
                    }
                }()
                return "Background saving started", nil
            },
            Spec: apicommand.Spec{MinArgs: 0, MaxArgs: 0},
        },
    }
}
```

Return types for `CommandHandler`:
- `string` → RESP simple string
- `int64` → RESP integer
- `[]byte` → RESP bulk string
- `nil` → RESP null
- `error` (second return) → RESP error

## Error Handling

| Scenario | Approach |
|----------|----------|
| Boot failure (corrupt file, I/O error) | Return error from `Boot()` — aborts server startup |
| Torn record on replay | Truncate at last-good offset, return `io.EOF` (recover intact prefix) |
| Transient Sink error (disk full) | Return error from `Apply()` — coordinator retries next batch |
| Fatal Sink error (unrecoverable) | Wrap error with `ErrSinkFatal` — coordinator quarantines the sink |
| Snapshot write failure | Return error from `SaveSnapshot()` — caller decides retry |

## Config and Hot Reload

Plugins receive config via `Build(cfg config.PluginConfig, ...)`. The `PluginConfig` is a `map[string]any` parsed from the server's YAML config. The plugin is responsible for extracting and validating its own keys — no library-specific types cross the boundary (ADR-0008).

For hot reload, set `Backend.OnReload` to a function that receives the updated config map. Common use: change fsync policy, rotate file paths, adjust buffer sizes.

## Testing

Test your plugin with the real `*cache.Cache` in `_test.go` files (test-only dependency — not compiled into the plugin binary):

```go
//go:build myplugin

package myplugin

import (
    "testing"
    "gocache/pkg/cache"
    apipersistence "gocache/api/persistence"
)

func TestWriteAndReplay(t *testing.T) {
    // 1. Create cache, write mutations via your Sink
    // 2. Close Sink
    // 3. Boot from your Source → replay iterator
    // 4. For each mutation: cache.ApplyMutation()
    // 5. Verify cache state matches expected
}
```

Run with your build tag and race detector: `go test -tags myplugin -race ./plugins/myplugin/...`

## Existing Plugins

| Plugin | Package | Interfaces | ADRs |
|--------|---------|------------|------|
| AOF | `plugins/aof` | Source + Sink + Commands (BGREWRITEAOF) | 0016, 0017 |
| Snapshot | `plugins/snapshot` | Source + Snapshotter + Commands (SAVE, BGSAVE, LASTSAVE) | 0005 |

## See Also

- [ADR-0001](../../adr/0001-persistence-as-pluggable-log-snapshot.md) — Persistence as pluggable log+snapshot+LSN
- [ADR-0002](../../adr/0002-source-sink-contract.md) — Source/Sink contract with BootMode trichotomy
- [ADR-0003](../../adr/0003-mutation-feed-and-fsync.md) — Mutation feed: group commit + fsync policy
- [ADR-0008](../../adr/0008-plugin-config-and-reload-contract.md) — Plugin config and reload contract
- [Plugins overview](../README.md) — Embedded vs IPC, build tags, scopes
- [Sequence: persistence flow](../../server/design/sequence/sequence_persistence.puml) — Generic persistence sequence diagram
- [Sequence: AOF plugin](../../server/design/sequence/sequence_aof.puml) — AOF-specific write/boot/recovery/rewrite flow
