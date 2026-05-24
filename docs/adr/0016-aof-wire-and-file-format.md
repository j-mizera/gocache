---
title: ADR-0016 AOF wire and file format
description: Append-only file uses a custom binary format (GOCAOF magic, varint-delimited mutation records, torn-write truncation recovery)
status: proposed
date: 2026-05-21
deciders: [witherxse]
related:
  - ADR-0001-persistence-as-pluggable-log-snapshot
  - ADR-0002-source-sink-contract
  - ADR-0003-mutation-feed-and-fsync
  - ADR-0005-snapshot-wire-and-file-format
---

# ADR-0016: AOF wire and file format

## Context

The persistence layer has a snapshot path (ADR-0005) but no mutation log. Snapshots alone mean the server can only recover to the last SAVE point — all mutations between snapshots are lost on crash. An append-only file (AOF) closes that gap by writing every mutation as it happens, enabling replay to the exact pre-crash state.

The mutation feed (ADR-0003) already delivers batches of `Mutation` structs to registered sinks via group-commit channels. An AOF sink needs a wire format to serialize those mutations to disk and a corresponding reader to replay them at boot.

The format must be:

1. **Append-friendly** — writes are sequential appends, never random-access updates.
2. **Recoverable** — a crash mid-write (torn write) must not corrupt previously written records.
3. **Simple** — the format is internal to gocache; cross-language portability is a non-goal (unlike snapshots, which third-party tools may produce).
4. **Compact** — the AOF can grow large; per-record overhead should be minimal.

## Decision

The AOF uses a **custom binary format** with these properties:

### File header (10 bytes)

| Offset | Size | Content |
|--------|------|---------|
| 0 | 6 | Magic bytes `GOCAOF` |
| 6 | 1 | Format version (`0x01`) |
| 7 | 3 | Reserved (zero-filled) |

### Record framing

Each record is varint-length-prefixed:

```
<varint body-length> <body>
```

Body layout:

| Field | Encoding |
|-------|----------|
| LSN | 8-byte little-endian uint64 |
| Op | varint op-length + op bytes (UTF-8 command name) |
| Args | varint arg-count, then per-arg: varint-length + arg bytes |

### Torn-write recovery

On boot, the reader walks forward from the first record. When it encounters a short read (body-length says N bytes but fewer remain) or a malformed record, it truncates the file at the last-good offset and returns `io.EOF`. No per-record checksum is needed — append-only semantics mean corruption can only occur at the tail, and truncation is the correct recovery.

### Determinism

Records are written in LSN order, inherited from the group-commit batch ordering in the mutation feed. Within a batch, mutations arrive in emission order (shard-lock serialized).

### Rewrite compaction (BGREWRITEAOF)

When the AOF grows large, BGREWRITEAOF compacts it:

1. Capture a snapshot of all live entries.
2. Create a temp file, write the AOF header.
3. For each entry, synthesize the equivalent mutation (SET for strings, HSET for hashes, SADD for sets, ZADD for sorted sets, RPUSH for lists) and write it as an AOF record.
4. Fsync the temp file.
5. Acquire the writer's mutex, rename temp over the current AOF, reopen the file handle, release the mutex.

The compacted file contains exactly one mutation per live key — the minimum set needed to reconstruct the current state.

## Alternatives Considered

### Alternative 1: RESP-format AOF (Redis style)

- **Pros**: Familiar to Redis users. The AOF is human-readable (RESP is text-based). Existing Redis AOF tools could parse it.
- **Cons**: RESP is a protocol format, not a storage format — re-parsing RESP on replay requires the full protocol parser. RESP encodes everything as strings, losing binary efficiency. No LSN field means replay ordering depends on file position alone. Redis compatibility is a protocol-level goal, not a storage-format goal.
- **Why not**: Binary framing is faster to encode/decode than RESP text. The AOF is an internal persistence detail; Redis tool compatibility doesn't justify the parse overhead.

### Alternative 2: Per-record CRC32

- **Pros**: Detects mid-record corruption (bit flips), not just truncation.
- **Cons**: Adds 4 bytes per record. For an append-only file, the only realistic failure mode is a torn write at the tail — the OS doesn't silently corrupt bytes in the middle of a completed `write(2)` + `fsync(2)` sequence. CRC validation on every record during replay adds CPU cost proportional to file size.
- **Why not**: Torn writes are detected by short reads (body-length exceeds remaining bytes). Mid-file bit flips are a storage-layer concern (ECC RAM, filesystem checksums) not an application-format concern. The complexity and overhead don't pay for themselves in the append-only case. If a future use case demands stronger integrity (e.g., archival shipping), a new format version can add it.

### Alternative 3: Protobuf records

- **Pros**: Schema-versioned. Cross-language decode for free.
- **Cons**: Adds a build dependency for the AOF reader/writer. Proto's varint encoding is similar to what we'd write by hand, but the surrounding framing (field tags, wire types) adds per-field overhead that's unnecessary when the record layout is fixed. The AOF format is internal — no external consumer needs to decode it.
- **Why not**: Same reasoning as ADR-0005's rejection of Protobuf for snapshots. The format is owned end-to-end; proto's schema-evolution machinery is overhead without benefit.

## Consequences

### Positive

- Crash recovery to the exact pre-crash state (within fsync policy bounds).
- Minimal per-record overhead: 8 bytes LSN + varints for framing. No CRC, no type tags, no footer.
- Torn-write recovery is trivial: walk forward, truncate at first bad record.
- BGREWRITEAOF keeps the file bounded — the AOF doesn't grow without limit.
- Format version in the header allows future evolution without breaking existing files.

### Negative

- Custom format means we own compatibility. A bug in the v1 reader/writer is on us.
- No per-record integrity check means silent mid-file corruption (extremely rare on modern hardware with ECC + journaling FS) would go undetected until the replayed state diverges. Acceptable for a cache; not acceptable for a database of record.
- Rewrite compaction requires a full snapshot capture, which briefly doubles memory pressure (same as BGSAVE).

### Risks

- **Risk**: Format-version-2 change breaks existing AOF files. **Mitigation**: Magic + version header lets the reader refuse unknown versions explicitly. Migration is write-a-new-file, not edit-in-place.
- **Risk**: BGREWRITEAOF rename races with the writer. **Mitigation**: The writer's mutex is held during the rename+reopen, so no concurrent writes can interleave.
- **Risk**: Very large AOF files slow down boot replay. **Mitigation**: BGREWRITEAOF compacts to one mutation per key. Operators can schedule periodic rewrites. Snapshot + AOF hybrid boot (snapshot for bulk, AOF for tail) is a future optimization documented in ADR-0001.
