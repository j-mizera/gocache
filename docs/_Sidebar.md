**[Home](Home)**

**Server**
- [Overview](Server)
- [Roadmap](Server-Roadmap)
- [Solution architecture](Server-Architecture)

**Plugins**
- [Overview](Plugins)
- [prometheus (IPC)](Plugin-Prometheus)

**Protocol**
- [GCPC v1](GCPC)

**Performance**
- [Overview](Performance)

**Decisions (ADR)**
- [Index](ADR)
- [0001 — Pluggable log+snapshot](ADR-0001-persistence-as-pluggable-log-snapshot)
- [0002 — Source/Sink contract](ADR-0002-source-sink-contract)
- [0003 — Mutation feed + fsync](ADR-0003-mutation-feed-and-fsync)
- [0004 — Command namespacing](ADR-0004-command-namespacing)
- [0005 — Snapshot wire format](ADR-0005-snapshot-wire-and-file-format)
- [0006 — Built-in vs IPC transport](ADR-0006-builtin-vs-third-party-transport)

**Diagrams (server)**
- [Components](Server-Components-Diagrams)
- [Sequences](Server-Sequence-Diagrams)
- [States](Server-State-Diagrams)

**Diagrams (GCPC)**
- [Components](GCPC-Components-Diagrams)
- [Sequences](GCPC-Sequence-Diagrams)
- [States](GCPC-State-Diagrams)

**Diagrams (prometheus)**
- [Components](Plugin-Prometheus-Components-Diagrams)
- [Sequences](Plugin-Prometheus-Sequence-Diagrams)

**Audits**
- [Per-shard arc summary](Audit-per-shard-arc-summary)
- [Go-vs-docker bench gap](Audit-go-bench-vs-docker-gap)
- [clientctx cross-goroutine](Audit-clientctx-cross-goroutine)
