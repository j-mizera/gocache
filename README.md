# GoCache

Redis-compatible in-memory cache server with a microkernel architecture. The core handles basic caching -- 69 commands across 5 data types. Everything else (Pub/Sub, Kafka, geospatial, auth, metrics, replication) runs as a plugin in a separate process. A crashing plugin cannot crash the core.

> Bachelor's thesis project exploring whether safe extensibility and high performance can coexist.

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
