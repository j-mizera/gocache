---
title: ADR-0005 Snapshot wire and file format
description: Snapshots use a custom binary format (varint, magic header, CRC32, optional zstd) replacing Go's gob encoder
status: accepted
date: 2026-05-03
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0002-source-sink-contract
  - Performance
---

# ADR-0005: Snapshot wire and file format

## Context

The current snapshot format is `encoding/gob`. Gob is convenient for Go-only interop and trivial to wire up — `gob.NewEncoder(file).Encode(state)` and the inverse on load. It's also the dominant fixed RSS cost in the persistence layer and has structural problems that show up under the new pluggable model.

The problems with gob in this role:

1. **Encoder state lives forever**: a single `gob.Encoder` accumulates type metadata for every type it has ever seen. The snapshot worker holds one for the process lifetime, so encoder memory grows monotonically.
2. **Decoder type-coupling**: gob bakes Go type names into the wire format. Renaming `pkg/cache.Item` to `api/cache.Item` (which is in scope for the api/-only refactor) breaks every existing snapshot file. There's no version negotiation.
3. **Not portable**: third-party providers in other languages (a Rust archival sink, a Python migration tool) can't read gob without writing a Go decoder. The pluggable persistence model assumes wire-format portability; gob actively works against it.
4. **No per-record framing**: gob streams are opaque; you can't seek, you can't validate one record without reading the whole file, and corruption mid-file makes the rest unreadable.
5. **No compression hook**: gob has no native compression. Adding it means wrapping the writer, which complicates streaming reads.

## Decision

Snapshots use a **custom binary format** with these properties:

- **Varint encoding** for all length-prefixed fields (keys, values, counts) — same shape as Protobuf and Redis RDB
- **Magic header** `GCDB\x01` (4-byte magic + 1-byte format-version) — version negotiation from byte 0
- **CRC32 footer** over the whole file — corruption detection on load
- **Per-record framing**: each record is `<varint-length><record-bytes>`, so partial-file recovery and incremental validation are possible
- **Optional zstd compression** at the record level, signalled by a per-record flag — compression is pluggable per record (text-heavy records compress; binary blobs may not)
- **Type-tagged records**: each record starts with a 1-byte type tag (`STRING`, `LIST`, `HASH`, `SET`, `ZSET`, `META`) — type metadata travels with the record, no global type registry needed

The format is documented in `api/persistence/format.md` and exercised by the built-in snapshot plugin. Third-party providers can use it directly or layer their own (e.g., Postgres source produces snapshot records via a custom path that constructs the same on-the-fly).

## Alternatives Considered

### Alternative 1: Keep gob

- **Pros**: Smallest diff. Already works for the existing snapshot worker.
- **Cons**: Every problem listed in Context above. The Go-only assumption blocks third-party portability; the global encoder state blocks the RSS reduction goal; the type-name coupling blocks the api/-only refactor.
- **Why not**: This is the format whose problems motivate the rewrite. Keeping it loses the thesis-line memory win.

### Alternative 2: Protobuf

- **Pros**: Battle-tested. Schema-versioned. Cross-language support out of the box. Codegen toolchain already in the project (used for GCPC).
- **Cons**: Schema-driven format adds a build dependency for every snapshot read — even third-party migration tools would need the .proto. Per-record framing is awkward (proto is record-oriented but doesn't natively chain). Compression is external. The on-the-wire size of a `repeated bytes` field for a large value is the same as the raw bytes plus a header — no win there. And proto's evolution rules (field numbers, reserved tags) are overkill for a format we control end-to-end.
- **Why not**: Protobuf earns its complexity when the schema is shared across many parties with independent release cycles. Gocache's snapshot is read by gocache plus migration tools we ship — the schema-evolution story is internal to one team. The ceremony doesn't pay for itself.

### Alternative 3: FlatBuffers

- **Pros**: Zero-copy reads. Well suited to mmap'd snapshot loads.
- **Cons**: FlatBuffers is even more schema-bound than Protobuf — the schema language and the codegen are non-trivial. Zero-copy reads are an interesting property for an in-memory cache, but the snapshot load path is one-shot at boot; per-cmd hot path doesn't touch it.
- **Why not**: Zero-copy doesn't help the boot path enough to pay for the schema toolchain. If the persistence layer ever does mmap-style hot reads (e.g., a "warm cache from snapshot file directly" mode), this is worth revisiting in a new ADR.

### Alternative 4: Redis RDB format

- **Pros**: Direct compatibility with Redis tooling. `redis-cli --rdb` style backup interop.
- **Cons**: RDB is undocumented for stability — it's an implementation detail of Redis, not a public format. Decoders exist (rdb-tools, etc.) but they tail the implementation. Tying gocache's persistence to a moving Redis-internal target is a long-term maintenance liability.
- **Why not**: The Redis-compatibility goal applies to the protocol (RESP, command semantics), not to the on-disk format. Adopting RDB would couple gocache to Redis's internals far beyond what protocol compat requires.

### Alternative 5: JSON with optional gzip

- **Pros**: Maximum portability. Human-readable for debugging.
- **Cons**: Catastrophic size and parse-cost overhead for binary values. Can't represent raw `[]byte` without base64 encoding (33% size penalty). Parse cost dominates load time on large snapshots.
- **Why not**: A cache's snapshot is binary-heavy by nature; JSON's text orientation is a structural mismatch.

## Consequences

### Positive

- Snapshot RSS no longer holds gob's encoder state for the process lifetime — encoders are local to one snapshot operation, GC'd after.
- Per-record framing enables partial-file recovery, snapshot validation tools, and (later) incremental snapshots.
- Magic + version header makes wire-format evolution explicit. We can add a new record type at version 2 without breaking version-1 readers.
- Format is documented in `api/persistence/format.md`, so third-party producers (e.g., a Postgres-source plugin generating snapshots on the fly) can target it directly.
- Optional zstd at record level gives the right knob — text-heavy records compress 3-5×; binary blobs skip compression.

### Negative

- A custom format means we own format compatibility forever. A bug in the version-1 reader is on us, not on a vendor.
- Migration from gob is not needed — the server boots via the Source contract and the GobSource shim remains available as a fallback reader. Users upgrading simply let the next SAVE write a v1 file; the old gob file can be deleted manually.
- Documentation cost: `api/persistence/format.md` becomes a living spec that must stay in sync with the implementation.

### Risks

- **Risk**: A format-version-2 change breaks existing tools. **Mitigation**: Magic + version header allows readers to refuse unknown versions explicitly. Migration tools are versioned. Old readers fail closed (refuse to load), not open (corrupt loads).
- **Risk**: CRC32 isn't strong enough — accepting a corrupted snapshot. **Mitigation**: CRC32 catches the overwhelming majority of disk-flip and truncation cases. For paranoia (e.g., archival use case), the format reserves room for a future stronger checksum signalled by version-bump.
- **Risk**: zstd library dependency. **Mitigation**: zstd is opt-in per record; uncompressed records work without the library. Pure-Go zstd implementations exist (`github.com/klauspost/compress/zstd`) — no cgo requirement.
