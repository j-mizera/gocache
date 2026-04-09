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

- Example plugins: auth (critical), metrics (non-critical), rate limiter (critical)
- Plugin-to-server cache read-back (allows plugins to query cache state)

## Phase 3: Observability

- OpenTelemetry tracing integration (span per command, propagation through plugin IPC)
- Health check HTTP endpoint (`/healthz`, `/readyz`)
- Prometheus metrics exporter plugin (command latency histograms, connection count, memory usage, hit/miss ratio)
- Custom metrics aggregation plugin

## Phase 4: Production Hardening

- AOF persistence (Append-Only File with fsync policies)
- Improved snapshot format (replace gob with a more robust binary format)
- Connection limits and backpressure
- Slow log (commands exceeding configurable latency threshold)
- Memory introspection (`MEMORY USAGE`, `MEMORY STATS`)
- Benchmarking suite vs Redis (redis-benchmark compatibility)

## Phase 5: Distributed Features (Plugins)

- Pub/Sub plugin: PUBLISH, SUBSCRIBE, PSUBSCRIBE, UNSUBSCRIBE
- Replication plugin: leader-follower with async replication
- Clustering plugin: hash slot sharding, MOVED/ASK redirects, consistent hashing

## Phase 6: Advanced Plugin Ecosystem

- OAuth2/OIDC authentication plugin (critical)
- Kafka bidirectional connector plugin
- Stream processing: XADD, XREAD, XRANGE, consumer groups
- Geospatial commands: GEOADD, GEODIST, GEORADIUS
- Full-text search plugin
- Time-series data plugin
- Lua scripting plugin: EVAL, EVALSHA, SCRIPT
