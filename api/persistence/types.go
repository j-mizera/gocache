// Package persistence is the public contract surface for gocache's
// persistence subsystem. Plugins under plugins/ may import this package;
// the corresponding implementation guts live in pkg/persistence/.
//
// The shape captured here is the operationalisation of the persistence ADRs:
//   - ADR-0001: persistence is pluggable, keyed by log + snapshot + LSN
//   - ADR-0002: split into Source (boot-side) and Sink (runtime-side)
//   - ADR-0003: per-Sink FsyncPolicy chosen at registration time
//
// The contract is transport-neutral — embedded plugins (built-in snapshot,
// built-in AOF) and IPC plugins (third-party connectors) implement the same
// types. ADR-0006 covers the transport choice.
package persistence

// LSN is a monotonic 64-bit Log Sequence Number. The coordinator allocates
// LSNs and tags every mutation with one; Sources surface the LSN at which
// a snapshot was taken so the runtime resumes allocation correctly.
//
// LSN 0 is reserved for "no LSN" — used by Sources that supply an Initial
// boot (no recovery state) or for fields that have not been set.
type LSN uint64

// ZeroLSN is the LSN value that means "no LSN" — never allocated by the
// coordinator. Used as a sentinel for boot results and uninitialised fields.
const ZeroLSN LSN = 0

// BootMode is what a Source declares when asked for recovery state. It is
// a closed trichotomy — see ADR-0002 for the rationale and ADR-0001 for
// the per-mode boot semantics.
type BootMode uint8

const (
	// BootModeInitial: the Source has nothing to recover from. The server
	// starts with an empty cache. The corresponding BootResult carries no
	// iterator and no LSN.
	BootModeInitial BootMode = iota

	// BootModeSnapshot: the Source supplies a point-in-time snapshot at
	// LSN=N. The server loads the snapshot, then resumes mutation
	// allocation from LSN=N+1. BootResult carries Snapshot and LSN.
	BootModeSnapshot

	// BootModeReplay: the Source supplies an iterator over the mutation
	// log starting from the lowest available LSN. The server replays each
	// mutation in order; the final LSN is the resume point. BootResult
	// carries Replay; the iterator carries the LSN per Mutation.
	BootModeReplay
)

// String renders BootMode for logs and errors.
func (m BootMode) String() string {
	switch m {
	case BootModeInitial:
		return "initial"
	case BootModeSnapshot:
		return "snapshot"
	case BootModeReplay:
		return "replay"
	default:
		return "unknown"
	}
}

// FsyncPolicy is a Sink's durability contract — when does it call fsync(2)
// (or the equivalent) on the underlying medium. Naming matches Redis's
// `appendfsync` so users carry the same mental model. See ADR-0003.
type FsyncPolicy uint8

const (
	// FsyncAlways: fsync after every group-committed batch. Highest
	// durability; highest latency. Equivalent to Redis appendfsync always.
	FsyncAlways FsyncPolicy = iota

	// FsyncEverySec: a background goroutine fsyncs at most once per second.
	// Loses up to ~1s of writes on crash. Default for built-in AOF.
	// Equivalent to Redis appendfsync everysec.
	FsyncEverySec

	// FsyncNo: no explicit fsync; rely on the OS page-cache flush schedule.
	// Loses up to ~30s on crash on Linux defaults. For dev, ephemeral
	// caches, or sinks where durability is delegated elsewhere.
	// Equivalent to Redis appendfsync no.
	FsyncNo
)

// String renders FsyncPolicy for logs and config dumps.
func (p FsyncPolicy) String() string {
	switch p {
	case FsyncAlways:
		return "always"
	case FsyncEverySec:
		return "everysec"
	case FsyncNo:
		return "no"
	default:
		return "unknown"
	}
}

// ValueType mirrors the cache's value-type tag at the contract layer so
// SnapshotEntry can carry it without leaking pkg/cache into api/. The
// values match pkg/cache.ValueType numerically; conversion at the boundary
// is a one-line cast in pkg/persistence.
type ValueType uint8

const (
	ValueTypeBytes     ValueType = iota // string / raw bytes
	ValueTypeList                       // list-of-strings
	ValueTypeHash                       // field→value map
	ValueTypeSet                        // unordered set
	ValueTypeSortedSet                  // score→member sorted set
)

// Encoding mirrors the cache's per-entry encoding flag (native Go shape vs
// packed byte buffer). Same boundary-conversion rule as ValueType.
type Encoding uint8

const (
	EncodingNative Encoding = iota // map / slice / *SortedSet
	EncodingPacked                 // flat []byte (pkg/cache/packed)
)

// Mutation is one durable write — what the cache committed under a single
// command. The coordinator allocates LSN and tags the Mutation before
// fanning it out to Sinks. The byte representation of Args mirrors RESP
// bulk-string framing so Sinks that persist commands verbatim (AOF, IPC
// replication) can write them out without re-encoding.
//
// Op is the command name uppercased (e.g. "SET", "HSET"). Key is the
// primary key for routing/sharding; for multi-key commands, Key is the
// first key (consistent with how the engine routes).
type Mutation struct {
	LSN  LSN
	Op   string
	Key  string
	Args [][]byte
}
