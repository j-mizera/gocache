package persistence

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"

	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
)

// GobSource adapts the existing gob-encoded snapshot format to the
// api/persistence.Source contract. It exists so the contract surface ships
// in feat/persistence-contract without forcing the new on-disk format
// (ADR-0005) to land at the same time. The new built-in snapshot plugin
// will replace this shim with the custom binary format.
//
// GobSource carries no LSN — gob snapshots predate the LSN concept. Boot
// returns LSN=ZeroLSN, which means the runtime allocator starts from 1.
// This is correct for the legacy path because gob is snapshot-only (no
// matching log); there's nothing to LSN-order against.
//
// Missing snapshot file → BootModeInitial. This matches the legacy
// LoadSnapshot semantics ("not found is not an error").
type GobSource struct {
	filename string
}

// NewGobSource returns a Source that reads from filename. The path is
// resolved at Boot time, not now — config hot-reload may change it
// between construction and boot.
func NewGobSource(filename string) *GobSource {
	return &GobSource{filename: filename}
}

// Name implements api/persistence.Source.
func (s *GobSource) Name() string { return "gob-snapshot" }

// Boot implements api/persistence.Source. Opens the snapshot file, reads
// the count header, and returns a SnapshotIterator that decodes one entry
// at a time. The iterator owns the file handle and closes it on Close.
func (s *GobSource) Boot(ctx context.Context) (apipersistence.BootResult, error) {
	file, err := os.Open(s.filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
		}
		return apipersistence.BootResult{}, fmt.Errorf("open %s: %w", s.filename, err)
	}

	dec := gob.NewDecoder(file)
	var count int
	if err := dec.Decode(&count); err != nil {
		_ = file.Close()
		// Empty file is an edge case — treat as Initial rather than an
		// error. The legacy LoadSnapshot also tolerated this implicitly
		// (the count decode would fail on EOF).
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
		}
		return apipersistence.BootResult{}, fmt.Errorf("decode header %s: %w", s.filename, err)
	}

	return apipersistence.BootResult{
		Mode: apipersistence.BootModeSnapshot,
		LSN:  apipersistence.ZeroLSN,
		Snapshot: &gobIterator{
			file:      file,
			dec:       dec,
			remaining: count,
		},
	}, nil
}

// gobIterator drains a gob-encoded SnapshotEntry stream. It expects exactly
// `remaining` entries — the count header is the contract. Reading more
// returns io.EOF; reading fewer returns an error from gob.
type gobIterator struct {
	file      *os.File
	dec       *gob.Decoder
	remaining int
	closed    bool
}

// Next yields the next SnapshotEntry, converting from the gob-internal
// SnapshotEntry shape to the api type. The conversion is a per-field cast
// because the enum values match by construction.
func (it *gobIterator) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	if it.closed {
		return apipersistence.SnapshotEntry{}, io.EOF
	}
	if it.remaining == 0 {
		return apipersistence.SnapshotEntry{}, io.EOF
	}
	var e SnapshotEntry
	if err := it.dec.Decode(&e); err != nil {
		if errors.Is(err, io.EOF) {
			return apipersistence.SnapshotEntry{}, io.EOF
		}
		return apipersistence.SnapshotEntry{}, fmt.Errorf("decode entry: %w", err)
	}
	it.remaining--
	return apipersistence.SnapshotEntry{
		Key:        e.Key,
		ValueType:  apipersistence.ValueType(e.ValueType),
		Encoding:   apipersistence.Encoding(e.Encoding),
		Value:      e.Value,
		Expiration: e.Expiration,
	}, nil
}

// Close releases the file handle. Idempotent — calling Close more than
// once returns nil after the first call.
func (it *gobIterator) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	return it.file.Close()
}

// Compile-time assertion: GobSource implements Source.
var _ apipersistence.Source = (*GobSource)(nil)

// Compile-time assertion: gobIterator implements SnapshotIterator.
var _ apipersistence.SnapshotIterator = (*gobIterator)(nil)

// Sentinel — use cache.Cache only via this file's narrow surface so a
// future refactor can swap the loader without re-grepping.
var _ = (*cache.Cache)(nil)
