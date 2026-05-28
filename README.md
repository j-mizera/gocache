# GoCache

Redis-compatible in-memory cache server with a microkernel architecture. The core handles basic caching — 75 commands across 5 data types over a per-shard locking design (default 8 shards, FNV-1a key routing). Everything else (Pub/Sub, Kafka, geospatial, auth, metrics, replication) runs as a plugin: most as separate processes via GCPC over Unix domain sockets, with a thin embedded-plugin tier (`-tags <name>`) for capabilities that must be active before config loads (crashdump, lifecycle OTLP). A crashing IPC plugin cannot crash the core.

> Bachelor's thesis project exploring whether safe extensibility and high performance can coexist. See `docs/audits/per-shard-arc-summary.md` for the throughput/RSS deltas and `docs/plugins/README.md` for the plugin contract.

## Quick Start

### Prerequisites

- Go 1.25.5+
- [Task](https://taskfile.dev/) (optional)

### Build & Run

```bash
task build
./bin/gocache-server
```

Or without Task:

```bash
go build -o bin/gocache-server ./cmd/server
./bin/gocache-server
```

Starts on `0.0.0.0:6379`. Connect with any Redis client, the included CLI, or netcat:

```bash
./bin/gocache-cli
redis-cli -p 6379
echo -e "PING\r\n" | nc localhost 6379
```

### Configuration

Copy and edit `gocache.yaml`. All settings can be overridden via CLI flags or `GOCACHE_*` env vars. Config changes (memory limits, log level, snapshot interval) are hot-reloaded without restart.

```bash
./bin/gocache-server --port 6380 --max-memory-mb 512 --log-level debug
```

## Development

```bash
task build          # Build server, cli, and plugins
task test           # Run tests with race detector
task test:coverage  # Tests with coverage report
task vet            # Static analysis
task proto          # Regenerate protobuf code
task version        # Print all artifact versions
```

## Documentation

- [docs/server/README.md](docs/server/README.md) — Server overview, configuration, supported commands, env vars.
- [docs/plugins/README.md](docs/plugins/README.md) — Plugin system: embedded vs IPC, build-tag matrix, the `api/`-only import rule.
- [docs/gcpc/README.md](docs/gcpc/README.md) — GCPC v1 protocol specification (Protobuf over Unix domain sockets).
- [docs/server/design/](docs/server/design/) and [docs/gcpc/design/](docs/gcpc/design/) — PlantUML diagrams (component, sequence, state).
- [docs/audits/](docs/audits/) — Performance audits and thesis anchors.
- [docs/performance/README.md](docs/performance/README.md) — Per-shard locking arc — shipped optimizations, measured deltas, remaining levers.
