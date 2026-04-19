# GoCache Server

The server is the core component of GoCache -- a Redis-compatible in-memory cache with a microkernel architecture. It handles RESP protocol parsing, command evaluation, memory management, persistence, and plugin orchestration.

## Building

```bash
task build:server
# or
go build -o bin/gocache-server ./cmd/server
```

## Running

```bash
./bin/gocache-server [flags]
```

### CLI Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--config` | Path to config file | `gocache.yaml` |
| `--address` | Listen address | `0.0.0.0` |
| `--port` | Listen port | `6379` |
| `--log-level` | Log level (trace, debug, info, warn, error, fatal) | `info` |
| `--max-memory-mb` | Maximum memory in MB | `1024` |
| `--eviction-policy` | Eviction policy (lru, noeviction) | `lru` |
| `--snapshot-file` | Snapshot file path | `snapshot.dat` |
| `--snapshot-interval` | Auto-snapshot interval | `5m` |
| `--load-on-startup` | Load snapshot on startup | `true` |
| `--cleanup-interval` | TTL cleanup sweep interval | `1m` |
| `--version` | Print version and exit | |

### Configuration

The server uses layered configuration with the following priority:

**CLI flags > Environment variables (`GOCACHE_*`) > Config file > Defaults**

```yaml
server:
  address: "0.0.0.0"
  port: 6379
  log_level: "info"
  require_pass: ""

persistence:
  snapshot_file: "snapshot.dat"
  snapshot_interval: 5m
  load_on_startup: true

memory:
  max_memory_mb: 1024
  eviction_policy: "lru"

workers:
  cleanup_interval: 1m

events:
  replay_capacity: 10000   # bounded ring for late subscribers; 0 disables

plugins:
  enabled: false
  dir: "bin/plugins"
  socket_path: "/tmp/gocache-plugins.sock"
  health_interval: 10s
  shutdown_timeout: 5s
  max_restarts: 3
  connect_timeout: 10s
  min_restart_interval_for_replay: 30s     # ophook replay skipped if plugin re-registers within window
  overrides:
    auth:
      failure_policy: halt_server          # canonical — replaces legacy `critical: true`
      priority: 1
      scopes: ["hook:pre", "read"]
```

Configuration is hot-reloadable via fsnotify. Changes to memory limits, eviction policy, snapshot interval, cleanup interval, and log level take effect without restart. Address and port changes require a restart.

### Embedded plugins (compile-time-linked)

A narrow set of capabilities must be active before config loads or before any IPC plugin can connect — crashdump collection, OTLP emission from t=0. These ship as **embedded plugins** linked in via build tags:

```bash
# Default: no embedded plugins (15 MB).
go build ./cmd/server

# With embedded crashdump scanner and OTLP exporter (~24 MB).
go build -tags "crashdump otlp" ./cmd/server

# Docker: per-variant image matrix published to GHCR on every main push.
docker pull ghcr.io/<org>/gocache:minimal   # no embedded
docker pull ghcr.io/<org>/gocache:default   # crashdump
docker pull ghcr.io/<org>/gocache:full      # crashdump + otlp
docker pull ghcr.io/<org>/gocache:latest    # alias for :default
```

Embedded-plugin env vars (all optional):

| Var | Default | Purpose |
|---|---|---|
| `GOCACHE_CRASHDUMP_DIR` | `crashes/` | Directory for JSON panic dumps |
| `GOCACHE_CRASHDUMP_DISABLED` | `false` | Skip crashdump writes (tests) |
| `GOCACHE_BOOT_STATE_FILE` | `boot.state` | Atomic stage marker file |
| `GOCACHE_EMBEDDED_OTLP_ENDPOINT` | _(unset)_ | Required for OTLP; unset = dormant |
| `GOCACHE_EMBEDDED_OTLP_SERVICE` | `gocache` | `service.name` resource attribute |
| `GOCACHE_EMBEDDED_OTLP_TIMEOUT_MS` | `3000` | Export timeout |
| `GOCACHE_EMBEDDED_OTLP_INSECURE` | auto | True for `http://` endpoints |
| `GOCACHE_EMBEDDED_OTLP_DISABLED` | `false` | Hard off-switch |

## Supported Commands

### String (16)

SET (NX/XX/EX/PX/KEEPTTL), GET, DEL, EXISTS, SETNX, EXPIRE, PEXPIRE, TTL, PTTL, INCR, DECR, INCRBY, DECRBY, INCRBYFLOAT, APPEND, STRLEN

### Multi-Key (2)

MGET, MSET

### List (8)

LPUSH, RPUSH, LPOP, RPOP, LLEN, LRANGE, BLPOP, BRPOP

### Hash (8)

HSET, HGET, HDEL, HEXISTS, HGETALL, HKEYS, HVALS, HLEN

### Set (9)

SADD, SREM, SMEMBERS, SISMEMBER, SCARD, SPOP, SINTER, SUNION, SDIFF

### Sorted Set (7)

ZADD, ZREM, ZSCORE, ZCARD, ZRANGE, ZRANK, ZCOUNT

### Transactions (5)

MULTI, EXEC, DISCARD, WATCH, UNWATCH

### Key Management (7)

TYPE, RENAME, RENAMENX, KEYS, SCAN, RANDOMKEY, OBJECT (ENCODING/IDLETIME/HELP)

### Server (10)

PING, ECHO, SELECT, QUIT, HELLO, AUTH, DBSIZE, INFO, FLUSHDB, FLUSHALL

### Persistence (2)

SNAPSHOT, LOAD_SNAPSHOT

### REX Metadata (1)

REX.META (SET/MSET/GET/DEL/LIST) -- connection-scoped metadata defaults. See the [GCPC REX section](../gcpc/README.md#rex-metadata) for the full protocol including `HELLO ... REXV 1` negotiation and per-command `META` directives.

**Total: 75 commands**

## Protocol Support

- RESP2 (default)
- RESP3 (negotiated via HELLO)
- Inline commands (telnet/netcat compatible)
- Pipelining (buffered writer, flush-on-drain)

## Authentication

Set `require_pass` in config or use `--require-pass` flag. When set, all commands except `AUTH` and `HELLO` are rejected until the client authenticates.

```
> AUTH mypassword
OK
```

## Graceful Shutdown

The server handles SIGTERM, SIGINT, and SIGHUP with a 6-step shutdown sequence:

1. Unblock all waiting BLPOP/BRPOP clients
2. Stop accepting connections, drain existing ones (10s timeout)
3. Shut down plugins (send Shutdown, wait for ack, force-kill)
4. Stop background workers (snapshot, TTL cleanup)
5. Save final snapshot
6. Stop engine

## Testing

```bash
# All server tests (unit + integration)
go test -race ./pkg/server/...

# Integration tests only
go test -race -run 'TestIT_' ./pkg/server/...

# Full suite
task test
```

## Design Documentation

- Server diagrams: `docs/server/design/` — components, sequences, state machines (including `state_embedded_plugin_lifecycle.puml`, `components_event_bus_ring.puml`, `sequence_boot_crash_survivability.puml`)
- GCPC protocol diagrams: `docs/gcpc/design/` (including `sequence_ophook_replay_on_subscribe.puml`, `state_ophook_replay_suppression.puml`)
- Full diagram index with links: [SOLUTION_ARCHITECTURE.md](SOLUTION_ARCHITECTURE.md#design-diagrams)
- GCPC protocol specification: [docs/gcpc/README.md](../gcpc/README.md)
