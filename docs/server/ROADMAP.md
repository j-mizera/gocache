# Server Roadmap

## Phase 1: Core Cache -- COMPLETE

- 5 data types: string, list, hash, set, sorted set
- 74 commands with Redis-compatible response formats
- RESP2/RESP3 protocol with per-connection negotiation
- Inline command support (telnet/netcat compatible)
- Pipelining with buffered writer
- Serial engine dispatch (all mutations through single goroutine)
- Transactions: MULTI/EXEC/DISCARD with atomic batch execution
- Optimistic locking: WATCH/UNWATCH with dirty-marking
- Blocking list operations: BLPOP/BRPOP with waiter registry
- LRU eviction with configurable memory limits and noeviction mode
- Dual TTL strategy: passive expiry on access + active background sweep
- Gob-based snapshot persistence with auto-save worker
- Basic AUTH (requirepass)
- Key introspection: OBJECT ENCODING/IDLETIME/HELP
- Configuration: YAML + CLI flags + env vars + hot reload via fsnotify
- Structured logging (zerolog)
- 6-step graceful shutdown
- Integration test suite (32 tests across 7 files)

## Phase 2: Plugin Framework -- IN PROGRESS

### Done

- GCPC v1 protocol: Protobuf over Unix domain sockets, length-prefixed framing
- IPC transport: write-mutex concurrency, stale socket cleanup
- Plugin lifecycle manager: fork/exec, health checks, restart policies, graceful shutdown
- Command registration: main namespace + REX (`PLUGIN:CMD`), shadow/duplicate protection, atomic registration
- Multiplexed IPC: correlation ID dispatch, concurrent command handling
- Hook system: pre/post command interception, priority-based execution, critical hooks can deny
- Permission/scope system: hierarchical scopes (admin > write > read), key namespace isolation, hook filtering
- Plugin SDK: Plugin, CommandPlugin, HookPlugin, ScopePlugin interfaces

### Next

- Metrics plugin (non-critical): Prometheus `/metrics` endpoint with command counters, latency histograms, memory usage
- Plugin-to-server cache read-back (allows plugins to query cache state)

## Phase 3: REX Metadata + Observability

### REX META -- Per-Request Metadata -- DONE

Stateless metadata directives attached to individual commands, not connection state. Metadata is discarded after each command executes.

```
META traceparent 00-abc123-def456-01
META authorization Bearer eyJhbG...
GET mykey
```

Enables:
- OpenTelemetry trace context propagation per-request (different operations on the same connection can belong to different traces)
- Per-request OAuth2 token validation (no stale tokens, no connection-scoped auth)
- Arbitrary plugin-defined metadata (tenant ID, request ID, priority hints)

Implementation:
- Capability negotiation: `HELLO 3 REXV 1` enables META lines
- Server read loop collects META lines into a temporary map, attaches to next command, discards after execution
- `REX.META` standalone command for connection-scoped sticky defaults (SET/MSET/GET/DEL/LIST)
- Precedence: per-command META > REX.META connection defaults
- Metadata injected into hook context under `shared.rex.` prefix — all plugins see it via existing `shared.` visibility
- Standard Redis clients unaffected (never send META, never negotiate REXV)

### Observability

- OpenTelemetry tracing via REX META context propagation (span per command, parent spans from client trace context)
- Health check HTTP endpoint (`/healthz`, `/readyz`)
- Upgrade metrics plugin with OTEL trace export
- Custom metrics aggregation

## Phase 4: Production Hardening

### Memory Optimization — Slab Allocator

Redesign the storage layer around a slab allocator pattern (inspired by Badger/Dgraph). The Go GC should only see a thin index of key → slab location. All entry data lives in GC-opaque, manually managed memory.

**Architecture**:

- **Slab allocator**: Fixed-size slab classes (64B, 256B, 1K, 4K, 16K, 64K+). Each slab class is a contiguous `mmap`'d region. Entries are placed in the smallest slab that fits. The GC never scans slab memory — it only sees the index map.
- **Key index**: `map[string]SlabPointer` is the only GC-visible structure. `SlabPointer` is a value type (slab class ID + offset), not a heap pointer. Key strings are the unavoidable GC cost; everything else is off-heap.
- **Entry layout**: Each slab entry is a flat byte layout: `[header: type, TTL, size, LRU timestamp][value bytes]`. No Go structs, no pointers — just raw bytes. Encode/decode at the slab boundary.
- **LRU via access timestamps**: Instead of a linked list (which creates GC-visible pointers), store last-access timestamps in the slab header. Eviction scans slab metadata directly — no heap allocation per access.
- **Byte-oriented hot path**: Keep values as `[]byte` from RESP parse through slab storage to RESP serialize. No `string` conversions on the hot path. RESP reader produces `[]byte`, slab stores `[]byte`, RESP writer consumes `[]byte`.
- **String pool**: Intern frequently-repeated strings (command names, common key prefixes) via a concurrent pool to reduce allocations for the key index.
- **Pre-allocated buffers**: `sync.Pool` for RESP read/write buffers and slab encode/decode scratch space.
- **Memory-mapped snapshots**: Snapshot loading via mmap — slab regions can be memory-mapped directly from the snapshot file without copying into heap.

### IPC Optimization — GCPC String Table

Reduce plugin IPC overhead by interning repeated strings at the GCPC protocol level:

- **Static table**: Server-defined constants embedded at compile time — command names (`GET`, `SET`, ...), hook context keys (`_start_ns`, `_elapsed_ns`, `shared.rex.*`), hook phases. Both sides know the table; no negotiation needed.
- **Dynamic table**: Grows during the plugin connection lifetime. When a string is first seen, it's sent in full and assigned an integer ID. Subsequent uses send only the ID. Similar to HTTP/2 HPACK compression.
- **Wire format**: `StringEntry { uint32 id; string value; }` exchanged inline or during registration. Context map keys and command names use IDs; values (JWTs, trace IDs) stay as full strings since they're unique per-request.
- **Priority**: Static table for command names and context keys first (biggest win, zero negotiation). Dynamic table later if profiling shows value.

### Persistence

- AOF persistence (Append-Only File with configurable fsync policies: always, everysec, no)
- Improved snapshot format (replace gob with a compact binary format, support partial loads)

### Operational

- Connection limits and backpressure (max connections, per-client command rate)
- Slow log (commands exceeding configurable latency threshold)
- Memory introspection (`MEMORY USAGE`, `MEMORY STATS`)
- Benchmarking suite vs Redis (redis-benchmark compatibility)

## Phase 5: Distributed Features (Plugins)

- Pub/Sub plugin: PUBLISH, SUBSCRIBE, PSUBSCRIBE, UNSUBSCRIBE
- Replication plugin: leader-follower with async replication
- Clustering plugin: hash slot sharding, MOVED/ASK redirects, consistent hashing

## Phase 6: Advanced Plugin Ecosystem

- OAuth2/OIDC authentication plugin (critical, uses REX META for per-request token validation)
- Kafka event streaming plugin (post-hooks on mutations, selective key namespace streaming to topics)
- Rate limiter plugin (GoCache as a rate limiting service: sliding window, token bucket, Lua-scriptable policies via REX commands)
- Stream processing: XADD, XREAD, XRANGE, consumer groups
- Geospatial commands: GEOADD, GEODIST, GEORADIUS
- Full-text search plugin
- Time-series data plugin
- Lua scripting plugin: EVAL, EVALSHA, SCRIPT
