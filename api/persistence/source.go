package persistence

import "context"

// Source supplies recovery state at boot time. Every persistence provider
// that contributes to recovery (built-in snapshot, built-in AOF, third-party
// Postgres connector, archival S3 reader, …) implements this interface.
//
// Source is invoked exactly once per server lifetime, before the cache
// accepts client traffic. After Boot returns, the Source's lifecycle is
// done — runtime mutations flow to Sinks (see Sink), not Sources.
//
// The split between Source and Sink is intentional: a provider can supply
// only one half if that's all it needs to do. A pure archival Sink doesn't
// have to write a Source stub; a one-shot Postgres-as-cache loader doesn't
// have to write a Sink stub. See ADR-0002.
type Source interface {
	// Name identifies the source for logging, error attribution, and
	// command-routing decisions (which Source answers SAVE, etc.).
	// Returns a stable identifier — typically the plugin / built-in name.
	Name() string

	// Boot returns the recovery artefact, or BootResult{Mode:Initial}
	// when there is nothing to recover. Errors propagate without retry —
	// boot failures abort startup.
	//
	// The returned BootResult owns its iterator(s); callers MUST close
	// the BootResult when done (BootResult.Close is the convenience
	// helper that closes whichever iterator was set).
	Boot(ctx context.Context) (BootResult, error)
}

// BootResult is what a Source returns from Boot. The Mode discriminator
// determines which other field is set:
//
//   - BootModeInitial: no iterator, LSN ignored.
//   - BootModeSnapshot: Snapshot is set; LSN is the snapshot's at-LSN
//     (the runtime resumes allocation from LSN+1).
//   - BootModeReplay: Replay is set; LSN is ignored on entry — the
//     resume LSN is whatever the iterator's last yielded Mutation
//     reported.
//
// Exactly one of Snapshot / Replay is non-nil for non-Initial modes.
type BootResult struct {
	Mode     BootMode
	LSN      LSN
	Snapshot SnapshotIterator
	Replay   ReplayIterator
}

// Close releases whichever iterator is held. Idempotent — safe to call
// multiple times. Callers should defer Close immediately after a successful
// Boot to ensure resources are released even on early-exit paths.
func (r BootResult) Close() error {
	if r.Snapshot != nil {
		return r.Snapshot.Close()
	}
	if r.Replay != nil {
		return r.Replay.Close()
	}
	return nil
}

// SnapshotIterator yields snapshot entries in source-defined order. The
// iterator is forward-only and single-pass; callers iterate to completion
// (Next returns io.EOF) or stop early via Close.
type SnapshotIterator interface {
	// Next returns the next entry, or io.EOF when exhausted.
	// On error other than io.EOF, the iterator is in an undefined state
	// and the caller should Close and abort recovery.
	Next(ctx context.Context) (SnapshotEntry, error)

	// Close releases any underlying resources (file handles, network
	// streams). Calling Close on an already-closed iterator is a no-op.
	Close() error
}

// SnapshotEntry is one key-value tuple in a snapshot. The Value field's
// concrete type depends on (ValueType, Encoding):
//
//   - EncodingPacked: Value is []byte — the packed byte buffer the cache
//     can hand straight to its slab allocator.
//   - EncodingNative + ValueTypeBytes: Value is []byte (legacy) or string
//     (older snapshots); the loader accepts both.
//   - EncodingNative + ValueTypeList: Value is []string.
//   - EncodingNative + ValueTypeHash: Value is map[string]string.
//   - EncodingNative + ValueTypeSet: Value is map[string]struct{}.
//   - EncodingNative + ValueTypeSortedSet: Value is provider-specific
//     (built-in encoder produces a serialisable form documented alongside
//     the format spec in ADR-0005).
//
// Expiration is the absolute Unix-nanosecond deadline; 0 means no expiry.
// The loader skips entries whose Expiration is in the past at load time.
type SnapshotEntry struct {
	Key        string
	ValueType  ValueType
	Encoding   Encoding
	Value      any
	Expiration int64
}

// ReplayIterator yields mutations in LSN order. Same lifecycle rules as
// SnapshotIterator (forward-only, single-pass, Close on early exit).
//
// Each yielded Mutation carries its own LSN. The coordinator advances its
// LSN cursor to the highest yielded LSN as replay progresses; the
// post-replay LSN is the resume point for runtime allocation.
type ReplayIterator interface {
	// Next returns the next mutation, or io.EOF when exhausted.
	Next(ctx context.Context) (Mutation, error)

	// Close releases any underlying resources. Idempotent.
	Close() error
}
