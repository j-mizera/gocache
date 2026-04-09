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

plugins:
  enabled: false
  dir: "bin/plugins"
  socket_path: "/tmp/gocache-plugins.sock"
  health_interval: 10s
  shutdown_timeout: 5s
  max_restarts: 3
  connect_timeout: 10s
  overrides:
    auth:
      critical: true
      priority: 1
      scopes: ["hook:pre", "read"]
```

Configuration is hot-reloadable via fsnotify. Changes to memory limits, eviction policy, snapshot interval, cleanup interval, and log level take effect without restart. Address and port changes require a restart.

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

**Total: 74 commands**

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

- Server diagrams: `docs/server/design/` (12 diagrams -- components, sequences, state machines)
- GCPC protocol diagrams: `docs/gcpc/design/` (14 diagrams)
- Full diagram index with links: [SOLUTION_ARCHITECTURE.md](SOLUTION_ARCHITECTURE.md#design-diagrams)
- GCPC protocol specification: [docs/gcpc/README.md](../gcpc/README.md)
