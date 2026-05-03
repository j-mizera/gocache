package persistence

import "context"

// Snapshotter persists a point-in-time dump of cache state on demand.
// Distinct from Sink (which consumes the ongoing mutation feed) and from
// Source (which supplies recovery state at boot): a snapshot is "stop
// the world for a moment, hand me every live entry, write it somewhere
// durable." That shape doesn't fit Sink (no LSN-ordered batching) and
// doesn't fit Source (Source is read-side, snapshot is write-side).
//
// Per ADR-0002, separate shapes get separate interfaces — a plugin that
// only consumes mutations (AOF, Kafka) doesn't have to stub-implement
// snapshot save, and a plugin that only saves snapshots doesn't have to
// stub-implement Apply.
//
// A single plugin implementation may register as both Source (to handle
// boot-time recovery from its on-disk format) and Snapshotter (to write
// new snapshots in that same format). The built-in gob shim does this
// today; the upcoming v1 binary plugin (ADR-0005) will too.
type Snapshotter interface {
	// Name identifies the snapshotter for logging and command routing
	// (which Snapshotter answers SAVE / BGSAVE). Returns a stable
	// identifier — typically the plugin / built-in name.
	Name() string

	// SaveSnapshot persists every entry yielded by src. The coordinator
	// builds src by iterating the live cache; the snapshotter sees only
	// SnapshotEntry values via Next, never the cache itself. The snapshotter
	// is responsible for atomicity: either the new snapshot fully replaces
	// the old, or the old remains intact (typical implementation: write
	// to a temp file, fsync, rename).
	//
	// Returns when src is exhausted (Next returns io.EOF) and durable
	// storage has been updated. Errors are surfaced to the caller without
	// retry — the caller (worker, SAVE handler, shutdown path) decides
	// the next move.
	SaveSnapshot(ctx context.Context, src SnapshotSource) error
}

// SnapshotSource yields snapshot entries on demand. Same lifecycle shape
// as SnapshotIterator (forward-only, single-pass, terminates on io.EOF)
// but consumed by Snapshotter.SaveSnapshot rather than by a boot-side
// loader. The two interfaces are kept distinct so each side's contract
// stays narrow — boot-side iteration is owned by the Source, save-side
// iteration is owned by the coordinator.
type SnapshotSource interface {
	// Next returns the next entry, or io.EOF when exhausted.
	// On error other than io.EOF, the source is in an undefined state
	// and the snapshotter should abort the save (typically by deleting
	// the temp file before returning).
	Next(ctx context.Context) (SnapshotEntry, error)
}

// LSNSeeder is an optional capability a Snapshotter may expose. The
// coordinator calls SetLSN with the current cursor right before
// SaveSnapshot so the snapshotter can embed the LSN in its on-disk
// format (the v1 binary format does this via a META record, ADR-0005).
//
// Snapshotters that don't carry LSN metadata in their format (e.g. the
// legacy gob shim, or third-party connectors that only write key-values)
// simply don't implement this interface — the coordinator skips the
// call via a type assertion.
type LSNSeeder interface {
	SetLSN(lsn LSN)
}
