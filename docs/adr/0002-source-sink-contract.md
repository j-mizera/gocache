---
title: ADR-0002 Source/Sink contract with BootMode trichotomy
description: Persistence contract splits into Source (recovery-side, supplies state on boot) and Sink (mutation-side, consumes ongoing writes), with a BootMode trichotomy of Snapshot/Replay/Initial
status: accepted
date: 2026-05-03
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0003-mutation-feed-and-fsync
  - ADR-0006-builtin-vs-third-party-transport
---

# ADR-0002: Source/Sink contract with BootMode trichotomy

## Context

ADR-0001 establishes that persistence is a pluggable subsystem keyed by log+snapshot+LSN. The next decision is the *shape* of the contract: what interfaces does a provider implement, and how do they cooperate with the server?

A persistence provider does two distinct jobs. On boot, it supplies whatever recovery state the server needs to reconstitute — a snapshot, a replay log, or a signal that there's nothing to load. During steady-state operation, it consumes the mutation feed as commands execute, and is responsible for whatever durability semantics the user signed up for (fsync-every-second, async write-behind, replicate to a remote endpoint, etc.).

These are different responsibilities with different lifetimes (boot-only vs always-on), different failure semantics (boot failure aborts startup; sink failure can be recoverable), and different concurrency requirements (boot is single-threaded by definition; sinks see the full mutation stream and must handle it without blocking the cache write path). Mashing them into one interface forces every provider author to think about both even when they only care about one.

## Decision

The contract splits into two interfaces in `api/persistence/`:

- **`Source`** — boot-side. Returns a `BootMode` indicating what kind of state it has, plus the corresponding artefact (snapshot reader, log replay iterator, or nothing). Used exactly once per server lifetime.
- **`Sink`** — runtime-side. Receives the ongoing mutation feed. Used for the entire server lifetime after boot.

Both interfaces share an LSN cursor so providers that implement both (the common case for built-ins) can advance the cursor coherently across snapshot + log.

`BootMode` is a closed trichotomy:

- `BootModeSnapshot` — provider supplies a point-in-time snapshot at LSN=N. Server loads the snapshot, then resumes the mutation feed from LSN=N+1.
- `BootModeReplay` — provider supplies an iterator over the mutation log starting from LSN=0 (or the lowest available LSN). Server replays each mutation in order.
- `BootModeInitial` — provider has nothing to load. Server starts from an empty cache.

A provider can implement both `Source` and `Sink` (built-ins do); third-party providers can implement only one (a pure archival sink, or a one-shot Postgres-as-cache loader).

## Alternatives Considered

### Alternative 1: Single bidirectional `Persistence` interface

- **Pros**: Fewer types to learn. One mental model.
- **Cons**: Forces every provider to implement boot logic even when it only ever consumes the mutation feed, and vice versa. Encourages no-op stubs. Harder to evolve — adding a method affects every implementor.
- **Why not**: The two responsibilities have different lifetimes and failure semantics; bundling them into one interface optimizes for the wrong axis.

### Alternative 2: Monolithic `Provider` plugin (Kafka-Connect style)

- **Pros**: Matches a familiar pattern (Kafka source/sink connectors). Each provider is one self-contained unit.
- **Cons**: The Connect framework's source/sink terminology actually matches what we're doing — splitting them is the *right* lesson from Connect, not the wrong one. A monolithic Provider would re-bundle them. Also: the user pushed back on the Kafka-Connect framing explicitly during plan review.
- **Why not**: User feedback during plan review was explicit: "it doesn't have to be kafka source/sink like, I want it to be efficient and a good design". The split-interface design is the efficient version of the same idea.

### Alternative 3: Boolean BootMode (snapshot vs no-snapshot)

- **Pros**: Fewer states to handle. Matches gob's current "load if file exists, else start empty" model.
- **Cons**: No room for a pure-replay provider (e.g., AOF without a snapshot, or a third-party WAL source). Forces every replay-style provider to fake a synthetic snapshot at LSN=0 — confusing and error-prone.
- **Why not**: Closed trichotomy is the smallest set of states that distinguishes the three legitimate boot paths. Two states forces unnatural workarounds.

### Alternative 4: `BootMode` as an open interface with custom modes

- **Pros**: Maximally flexible. Future providers could add new modes (e.g., `BootModePartial` for incremental boot).
- **Cons**: Open enums prevent the server from exhaustively handling all cases at boot time. Every new mode would need server-side dispatch. Defeats the point of a closed contract.
- **Why not**: Boot complexity should be bounded; if a fourth mode is genuinely needed later, it can be added to the closed enum (and that's a contract revision worth its own ADR).

## Consequences

### Positive

- Provider authors only implement the half they care about. A pure archival sink doesn't have to write a `Source` stub.
- Boot failure semantics are localized to `Source` — sinks can fail and recover without aborting the server.
- The `BootMode` trichotomy makes server-side boot dispatch exhaustive: a `switch BootMode` covers every case at compile time.
- Built-ins (snapshot, AOF) implement both interfaces and share the LSN cursor coherently.

### Negative

- Two interface types instead of one. Slightly more surface to document.
- Providers that implement both have to maintain LSN coordination between their `Source` and `Sink` halves. The contract documents this; the built-ins demonstrate the pattern.

### Risks

- **Risk**: A real-world provider needs a fourth boot mode we didn't anticipate. **Mitigation**: Closed trichotomy is intentional — adding a fourth mode is an explicit contract revision (new ADR). The cost of "I have to write a new ADR" is the right gate for "we're broadening the contract".
- **Risk**: Providers implement `Source` and `Sink` with subtly inconsistent LSN semantics. **Mitigation**: The contract documents the LSN invariants; the built-in implementations are the reference. ADR-0003 covers the mutation-feed side of the same coordination.
