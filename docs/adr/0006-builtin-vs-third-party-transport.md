---
title: ADR-0006 Built-in vs third-party plugin transport
description: Built-in persistence plugins (snapshot, AOF) ship as embedded plugins (in-process, build-tag-gated); third-party providers run via IPC over GCPC
status: accepted
date: 2026-05-03
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0002-source-sink-contract
  - ADR-0008-plugin-config-and-reload-contract
  - Plugins
  - GCPC
---

# ADR-0006: Built-in vs third-party plugin transport

## Context

Once persistence is pluggable (ADR-0001) with a Source/Sink contract (ADR-0002), the next question is *how* a plugin runs. Gocache already has two plugin transports:

- **Embedded plugins** — compiled into the server binary, gated by Go build tags (e.g., `-tags crashdump,otlp`). Same process; no IPC overhead. Examples: `crashdump`, `otlp`. Activation is at build time.
- **IPC plugins** — separate OS processes connected over Unix domain sockets, speaking GCPC v1 (Protobuf). Enabled via YAML at runtime. Example: `gobservability`. Crash isolation between server and plugin.

Persistence is unique among plugin domains because it sits squarely on the durability path. A snapshot or AOF crash that loses writes is much worse than a metrics-plugin crash that loses telemetry. At the same time, the third-party use cases enabled by ADR-0001 — Postgres-as-cache, Kafka archival, S3 replication — are exactly the cases where running an external connector in a separate process is the *right* shape (different runtime, different deploy cycle, different blast radius).

The two transports have different strengths. Embedded plugins win on latency, simplicity (no IPC framing), and bootstrapping (active before config loads). IPC plugins win on isolation, language-flexibility (an IPC plugin could be Rust, Python, anything that speaks Protobuf), and deploy-independence (update the plugin without restarting the server).

## Decision

Built-in persistence plugins **ship as embedded plugins** (compile-time-linked, build-tag-gated):

- `snapshot` build tag — built-in snapshot Source/Sink (the gob replacement using ADR-0005's format)
- `aof` build tag — built-in AOF Source/Sink with the group-commit/fsync model from ADR-0003

Third-party persistence providers **run as IPC plugins** over GCPC, implementing the same `api/persistence/` Source/Sink contract surfaced through the GCPC v1 protocol's plugin-callable methods. The contract is transport-neutral — a Source method has the same signature whether it's invoked in-process (embedded) or over a UDS (IPC).

Default release builds include `snapshot` (matches the existing default behaviour). `aof` is opt-in. Stripped builds (`-tags ''`) ship without persistence and rely on a configured IPC provider — useful for niche deployments (test rigs, ephemeral caches, external-source-of-truth setups).

## Alternatives Considered

### Alternative 1: All persistence plugins as IPC

- **Pros**: One transport story. Maximum isolation — a snapshot plugin crash doesn't take down the server.
- **Cons**: Built-in snapshot becomes an external process the user has to deploy alongside the server. Default deployment goes from "run the binary" to "run the binary AND the snapshot sidecar". The latency floor on every persistence operation is now a UDS round-trip. The bootstrapping problem: snapshot Source has to run *before* config is fully loaded (to recover state), which fights the IPC plugin lifecycle (IPC plugins start after config).
- **Why not**: The default deployment story matters. Users picking up the binary expect it to persist out of the box; making them wire up an external snapshot daemon for the basic case is a significant regression.

### Alternative 2: All persistence plugins as embedded

- **Pros**: One transport story (the other direction). Lowest possible latency for every plugin.
- **Cons**: Third-party providers (Postgres, S3, Kafka) would have to be linked into the server binary at build time. Every deployment that wants a third-party connector has to cut a custom binary. Crash isolation is gone — a Postgres connector bug crashes the cache.
- **Why not**: The third-party use cases enabled by ADR-0001 are exactly the cases where the connector is heavy, language-diverse, and benefits from process isolation. Forcing them in-process undoes the value.

### Alternative 3: Auto-select transport based on a per-plugin policy

- **Pros**: Flexibility. A plugin author could ship the same code to run either embedded or IPC.
- **Cons**: The Source/Sink contract is already transport-neutral; the *deployment model* of a given plugin is what differs. Auto-selection means the plugin author writes the same code twice (for cgo restrictions, runtime dependencies, etc.) and the user has to understand which mode they're in.
- **Why not**: The transport choice is a property of the plugin's deployment shape, not the user's runtime config. Built-ins ship as embedded because we control their lifecycle; third-parties ship as IPC because they control theirs. Auto-select adds complexity for no user benefit.

### Alternative 4: Embedded built-ins with optional IPC mode for development

- **Pros**: Same as the chosen decision, plus an IPC fallback for users who want to debug snapshot/AOF in isolation.
- **Cons**: Two activation paths for the same plugin (build tag + YAML). Doubled test surface. The "optional IPC mode" is rarely used and rots when the embedded path is the hot path.
- **Why not**: If a user wants to debug snapshot/AOF in isolation, they can build a one-off binary that omits the embedded version and add an IPC sidecar that wraps the same code. The contract is transport-neutral; the test rig can use it either way without us shipping a second activation path.

## Consequences

### Positive

- Default deployment ("run the binary") works with snapshot persistence out of the box — no sidecars, no extra processes.
- Third-party connectors plug in as IPC plugins over GCPC, with full crash isolation and language flexibility.
- Build-tag matrix grows by 2 (`snapshot`, `aof`) — small enough to keep the existing CI matrix tractable.
- The Source/Sink contract being transport-neutral means the same `api/persistence/` types describe both built-in and third-party plugins. One mental model.
- AOF being opt-in means users who don't want it pay nothing for it (no goroutines, no buffer allocations, no fsync ticker).

### Negative

- Built-in plugins crash the server if they crash. Snapshot/AOF code has to meet a higher reliability bar than the metrics plugin does.
- Two transport paths means two test paths. The contract is shared, but the GCPC marshalling for IPC and the in-process direct call for embedded both need exercise.
- Build matrix complexity: a release with `crashdump,otlp,snapshot,aof` is the "full" build; a release without any of these is the "minimal" build; users picking subsets get something in between. CI has to cover at least the corners.

### Risks

- **Risk**: A built-in plugin's bug crashes the server *because* it's embedded. **Mitigation**: Built-in plugins are held to higher reliability — extra test coverage, defensive error handling around the durability path, code review focused on the crash blast radius. Same standard as the cache core.
- **Risk**: A third-party IPC plugin's crash leaves the server in a state where Sinks have stale state. **Mitigation**: ADR-0003's group-commit model already handles bounded backpressure; the coordinator detects sink failure and surfaces it. The contract documents that Sinks can fail and recover; users running flaky third-party connectors should expect occasional log noise, not silent data loss.
- **Risk**: GCPC's IPC overhead makes third-party persistence sinks too slow for high-throughput workloads. **Mitigation**: Group-commit batching (ADR-0003) amortises the per-flush IPC cost. For workloads where even batched IPC is too slow, the alternative is to write an embedded plugin (i.e., rebuild the server with the connector code linked in). The escape hatch exists.
