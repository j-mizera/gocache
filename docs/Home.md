# GoCache

Redis-compatible in-memory cache server with a microkernel architecture. The core handles 75 commands across 5 data types over a per-shard locking design (default 8 shards, FNV-1a key routing). Everything else (Pub/Sub, Kafka, geospatial, auth, metrics, replication, persistence) runs as a plugin — most as separate processes via GCPC over Unix domain sockets, with a thin embedded-plugin tier for capabilities that must be active before config loads.

> Bachelor's thesis project exploring whether safe extensibility and high performance can coexist.

This wiki is **auto-generated** from `docs/` on every push to `main`. To edit a page, edit the corresponding file in the repo's `docs/` directory and open a PR.

## Where to start

- **[Server](server/README)** — Server overview, configuration, supported commands, env vars, embedded plugins.
- **[Plugins](plugins/README)** — Embedded vs IPC plugins, build-tag matrix, the `api/`-only import rule, available plugins.
- **[GCPC protocol](gcpc/README)** — GCPC v1 specification (Protobuf over Unix domain sockets) — the contract IPC plugins implement.
- **[Audits](audits)** — Performance audits and thesis anchors (per-shard arc summary, Go-vs-docker bench gap).
- **[Design diagrams](server/design)** — Component, sequence, and state diagrams in PlantUML.

## Active development

| Phase | Status | Tracking |
|---|---|---|
| Phase 1 — Core cache | ✅ Complete | 75 commands, RESP2/3, transactions, persistence (gob), AUTH, OBJECT introspection, hot reload |
| Phase 2 — Plugin framework | ✅ Complete | GCPC v1, IPC transport, command routing, hooks, scopes, query channels, operation hooks |
| Phase 3 — REX metadata + Operations | ✅ Complete | Per-request metadata, server queries, operation tracker, replay-on-subscribe |
| Phase 4 — Production hardening | 🔄 In progress | Memory optimization (slab + GC-opaque LRU) ✅; sink-aware fast path ✅; per-shard locking ✅; engine pooling ❌; read-lock bypass ❌; persistence-as-plugin ⏳ |
| Phase 5 — Advanced plugins | ❌ Not started | OAuth2, Kafka, ratelimit, cluster |

Detailed phase tracker: [`.claude/IMPLEMENTATION_STATUS.md`](https://github.com/j-mizera/gocache/blob/main/.claude/IMPLEMENTATION_STATUS.md) in the repo (not wiki — kept on `main` so it tracks merge state precisely).

## Project layout

```
api/        Public contract surface — plugins import only from here
sdk/        Plugin author SDK
cmd/        Entry points (server, cli)
pkg/        Server internals — depends on api/, never on sdk/, never imported by plugins/
plugins/    Plugin implementations (embedded + IPC)
docs/       This wiki — auto-synced on push to main
bench/      Benchmarks and results per branch
scripts/    Build/test/CI helpers
```

## Build

```bash
# Default (no embedded plugins, ~15 MB binary)
go build -o bin/gocache-server ./cmd/server

# With embedded plugins (crashdump + OTLP from t=0)
go build -tags "crashdump otlp" -o bin/gocache-server ./cmd/server

# With pprof endpoint enabled
go build -tags "crashdump otlp pprof" -o bin/gocache-server ./cmd/server

# Docker (multi-arch, signed, on every main push)
docker build --build-arg PLUGINS=crashdump,otlp -t gocache:full .
```

See [Server / build](server/README#building) for details.
