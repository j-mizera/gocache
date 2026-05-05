// Package snapshot implements the v1 snapshot wire and file format
// described in ADR-0005. It is the successor to the legacy gob-encoded
// snapshot (pkg/persistence.GobSource) and ships behind a config flag
// (persistence.format: v1) so deployments can opt in independently of
// the main release cycle.
//
// # Wire layout
//
//	+----------------------+------------------+ ... +------------------+
//	| header (5 bytes)     | record           |     | record           |
//	+----------------------+------------------+ ... +------------------+
//	| 'G' 'C' 'D' 'B' 0x01 | varint-len, body | ... | varint-len, body |
//	+----------------------+------------------+ ... +------------------+
//	                                                | crc32 (4 bytes)  |
//	                                                +------------------+
//
// Header is fixed at 5 bytes:
//
//   - bytes 0..3: ASCII magic "GCDB"
//   - byte  4   : format version (currently 0x01)
//
// Each record is a length-prefixed body. The varint encodes the body
// byte length; readers can skip an unknown record type by reading the
// length and seeking past it.
//
// CRC32 (IEEE polynomial, little-endian) is appended after the last
// record. The checksum covers every byte before it (header + records).
// On read, the iterator's Close method validates the CRC against the
// computed value; corrupted files surface the failure on Close rather
// than silently returning partial data.
//
// # Record body
//
//	+--------+---- type-specific payload ----+
//	| typeT  | (decoded by record type)      |
//	+--------+-------------------------------+
//
// The first byte of every body is the type tag. Defined values:
//
//   - 0x00  TypeMeta    — stream metadata (LSN cursor, future flags)
//   - 0x01  TypeString  — apipersistence.ValueTypeBytes
//   - 0x02  TypeList    — apipersistence.ValueTypeList
//   - 0x03  TypeHash    — apipersistence.ValueTypeHash
//   - 0x04  TypeSet     — apipersistence.ValueTypeSet
//   - 0x05  TypeZSet    — apipersistence.ValueTypeSortedSet
//
// Unknown type tags are not silently skipped — the reader returns an
// error so a forward-compatible stream consumed by an older binary
// fails closed rather than dropping data on the floor.
//
// # Data record payload (TypeString..TypeZSet)
//
//	+-------+-----------+--------------+--------+-----+----------------+--------+-----+
//	| flags | encoding  | varint expir | varint | key | varint val-len | val    |     |
//	|       |           |              | key-ln |     |                | (blob) |     |
//	+-------+-----------+--------------+--------+-----+----------------+--------+-----+
//
// Fields:
//
//   - flags     (1 byte): bit 0 = value blob is zstd-compressed
//   - encoding  (1 byte): 0 = native, 1 = packed (slab-allocator buffer)
//   - expir     (varint): expiration in unix-nanoseconds; zero means none
//   - key-ln    (varint): byte length of the key
//   - key       (bytes) : the key
//   - val-len   (varint): byte length of the value blob (post-compression)
//   - val       (bytes) : value blob, decoded according to type+encoding
//
// Per-record zstd lets text-heavy values compress 3–5× while binary
// blobs skip compression entirely. The compress-decision threshold lives
// on the writer (see writer.go); the reader treats it as a per-record
// flag and decompresses whenever bit 0 is set.
//
// # Value blob shapes
//
// EncPacked (encoding == 1) for any type: the slab-allocator buffer is
// stored verbatim. Cache.RawLoadPacked reads it back; the cache layer
// is the source of truth for packed wire shapes.
//
// EncNative (encoding == 0):
//
//   - TypeString : raw bytes
//   - TypeList   : varint count + count×(varint len + bytes)
//   - TypeHash   : varint count + count×(varint key-len + key + varint val-len + val) — entries lex-sorted by key for determinism
//   - TypeSet    : varint count + count×(varint len + bytes) — members lex-sorted
//   - TypeZSet   : varint count + count×(varint member-len + member + 8B float64 score) — sorted by (score, member)
//
// Sorting native collections is a determinism property: two snapshots
// of the same logical state produce byte-identical files, which makes
// CI golden-file diffs and external dedup (rsync, content-addressed
// storage) work as expected.
//
// # META record payload (TypeMeta)
//
//	+--------+----- sub-tag dispatch ------+
//	| subT   | (decoded by sub-tag)        |
//	+--------+-----------------------------+
//
// The first byte after the type tag is a sub-tag. Defined values:
//
//   - 0x01  MetaSubLSN — payload is a varint LSN cursor
//
// Future stream-wide metadata (record count, build version, compression
// stats) lives in additional sub-tags. Unknown sub-tags are tolerated:
// the reader skips them via the outer record's varint length so older
// readers stay forward-compatible with new META variants.
package snapshot

// File-format constants. Not exported as "FormatV1" wraps everything:
// callers don't construct format bytes themselves, they go through
// NewSource / NewSnapshotter which embed the right values.
const (
	// Magic is the 4-byte ASCII signature at offset 0.
	magic0 byte = 'G'
	magic1 byte = 'C'
	magic2 byte = 'D'
	magic3 byte = 'B'

	// Version is the 1-byte format version at offset 4.
	// Bumping this is a wire-format change — old readers refuse to
	// load. Migration is via the gocache-migrate CLI.
	formatVersion byte = 0x01

	// HeaderLen is the fixed prefix size: 4 magic bytes + 1 version byte.
	headerLen = 5

	// FooterLen is the trailing CRC32 size.
	footerLen = 4
)

// Type tags. The numeric values are stable wire-format and must not
// shift across version-1 readers.
const (
	typeMeta   byte = 0x00
	typeString byte = 0x01
	typeList   byte = 0x02
	typeHash   byte = 0x03
	typeSet    byte = 0x04
	typeZSet   byte = 0x05
)

// META sub-tags.
const (
	metaSubLSN byte = 0x01
)

// Per-record flags (bit positions).
const (
	flagZstd byte = 1 << 0
)

// Encoding wire values. These mirror api/persistence.Encoding so the
// cast at the boundary is a numeric round-trip.
const (
	encNative byte = 0x00
	encPacked byte = 0x01
)

// compressionThreshold is the value-blob byte length at or above which
// the writer attempts zstd compression. Below this size, compression
// metadata + zstd's per-block overhead typically outweigh the savings.
//
// The exact value is a heuristic — picked low enough to compress
// medium-text values (1KB JSON, etc.) and high enough that small string
// keys-as-values stay raw. Tunable in a future config knob if a
// workload reveals a better threshold.
const compressionThreshold = 256

// minCompressionGain is the minimum byte-savings required for the
// compressed payload to win over the raw payload. Without this gate, a
// payload that compresses by 1 byte still pays the per-record framing
// for the compressed flag — net loss. 16 bytes is conservative.
const minCompressionGain = 16
