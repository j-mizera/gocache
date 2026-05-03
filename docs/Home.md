---
title: Home
description: GoCache wiki landing — entry point to server, plugins, GCPC, performance, audits, and design diagrams
status: living
last_updated: 2026-05-03
related:
  - Server
  - Plugins
  - GCPC
  - Performance
---

# GoCache

Redis-compatible in-memory cache server with a microkernel architecture. The core handles 75 commands across 5 data types over a per-shard locking design (default 8 shards, FNV-1a key routing). Everything else (Pub/Sub, Kafka, geospatial, auth, metrics, replication, persistence) runs as a plugin — most as separate processes via GCPC over Unix domain sockets, with a thin embedded-plugin tier for capabilities that must be active before config loads.

> Bachelor's thesis project exploring whether safe extensibility and high performance can coexist.

This wiki is **auto-generated** from `docs/` on every push to `main` via `scripts/build-wiki.sh`. To edit a page, edit the corresponding file in the repo's `docs/` directory and open a PR.

## Where to start

- **[Server](Server)** — Server overview, configuration, supported commands, env vars, embedded plugins.
- **[Plugins](Plugins)** — Embedded vs IPC plugins, build-tag matrix, the `api/`-only import rule, available plugins.
- **[GCPC protocol](GCPC)** — GCPC v1 specification (Protobuf over Unix domain sockets) — the contract IPC plugins implement.
- **[Performance](Performance)** — Per-shard locking arc — shipped optimizations, measured deltas, structural caps, remaining levers.
- Audits (cross-cutting — performance, design, races):
  - [Per-shard arc summary](Audit-per-shard-arc-summary)
  - [Go-vs-docker bench gap](Audit-go-bench-vs-docker-gap)
  - [clientctx cross-goroutine](Audit-clientctx-cross-goroutine)
- Design diagrams (server, rendered SVGs):
  - [Components](Server-Components-Diagrams)
  - [Sequences](Server-Sequence-Diagrams)
  - [States](Server-State-Diagrams)
- Design diagrams (GCPC):
  - [Components](GCPC-Components-Diagrams)
  - [Sequences](GCPC-Sequence-Diagrams)
  - [States](GCPC-State-Diagrams)
- Design diagrams (gobservability plugin):
  - [Components](Plugin-Gobservability-Components-Diagrams)
  - [Sequences](Plugin-Gobservability-Sequence-Diagrams)

## Active development

| Phase | Status | Tracking |
|---|---|---|
| Phase 1 — Core cache | ✅ Complete | 75 commands, RESP2/3, transactions, persistence (gob), AUTH, OBJECT introspection, hot reload |
| Phase 2 — Plugin framework | ✅ Complete | GCPC v1, IPC transport, command routing, hooks, scopes, query channels, operation hooks |
| Phase 3 — REX metadata + Operations | ✅ Complete | Per-request metadata, server queries, operation tracker, replay-on-subscribe |
| Phase 4 — Production hardening | 🔄 In progress | Memory optimization (slab + GC-opaque LRU) ✅; sink-aware fast path ✅; per-shard locking ✅; engine pooling ❌; read-lock bypass ❌; persistence-as-plugin ⏳ |
| Phase 5 — Advanced plugins | ❌ Not started | OAuth2, Kafka, ratelimit, cluster |

## Project layout

```
api/        Public contract surface — plugins import only from here
sdk/        Plugin author SDK
cmd/        Entry points (server, cli)
pkg/        Server internals — depends on api/, never on sdk/, never imported by plugins/
plugins/    Plugin implementations (embedded + IPC)
docs/       Source for this wiki — auto-flattened by scripts/build-wiki.sh on push
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

See [Server / build](Server#building) for details.
