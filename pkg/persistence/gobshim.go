package persistence

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"gocache/commons/logger"
	apipersistence "gocache/api/persistence"
)

// GobSource adapts the existing gob-encoded snapshot format to the
// api/persistence.Source and api/persistence.Snapshotter contracts. It
// exists so the contract surface ships in feat/persistence-contract
// without forcing the new on-disk format (ADR-0005) to land at the same
// time. The new built-in snapshot plugin will replace this shim with the
// custom binary format.
//
// GobSource carries no LSN — gob snapshots predate the LSN concept. Boot
// returns LSN=ZeroLSN, which means the runtime allocator starts from 1.
// This is correct for the legacy path because gob is snapshot-only (no
// matching log); there's nothing to LSN-order against.
//
// Missing snapshot file → BootModeInitial. This matches the legacy
// LoadSnapshot semantics ("not found is not an error").
//
// Filename is hot-reloadable via SetFilename: config reload can swap
// the snapshot path without recreating the shim or restarting the
// coordinator.
type GobSource struct {
	mu       sync.RWMutex
	filename string
}

// NewGobSource returns a shim that reads from and writes to filename.
// The path is resolved at Boot / SaveSnapshot time, not now — config
// hot-reload may change it between construction and use via SetFilename.
func NewGobSource(filename string) *GobSource {
	return &GobSource{filename: filename}
}

// SetFilename updates the on-disk path. Concurrent with in-flight Boot or
// SaveSnapshot calls — the path-read in those methods grabs an RLock so
// the change is observed atomically.
func (s *GobSource) SetFilename(f string) {
	s.mu.Lock()
	s.filename = f
	s.mu.Unlock()
}

// currentFilename returns the active path. Held under RLock so config
// reload sees a consistent snapshot of the field.
func (s *GobSource) currentFilename() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.filename
}

// Name implements api/persistence.Source and api/persistence.Snapshotter.
func (s *GobSource) Name() string { return "gob-snapshot" }

// Boot implements api/persistence.Source. Opens the snapshot file, reads
// the count header, and returns a SnapshotIterator that decodes one entry
// at a time. The iterator owns the file handle and closes it on Close.
func (s *GobSource) Boot(ctx context.Context) (apipersistence.BootResult, error) {
	filename := s.currentFilename()
	file, err := os.Open(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
		}
		return apipersistence.BootResult{}, fmt.Errorf("open %s: %w", filename, err)
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
		return apipersistence.BootResult{}, fmt.Errorf("decode header %s: %w", filename, err)
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

// SaveSnapshot implements api/persistence.Snapshotter. It drains src into
// memory (the gob format requires a count header up-front so streaming
// would need a separate pass anyway), writes to a sibling temp file,
// fsyncs, and renames over the destination atomically. Crash mid-write
// leaves the previous snapshot intact instead of corrupting it.
//
// Mirrors the legacy persistence.SaveSnapshot logic — the only behavioural
// difference is the input shape (SnapshotSource pulls vs cache.Cache.Range
// callback). Conversion from api/persistence.SnapshotEntry to the
// gob-internal SnapshotEntry is per-field, matching the Boot-side
// conversion in reverse.
func (s *GobSource) SaveSnapshot(ctx context.Context, src apipersistence.SnapshotSource) error {
	filename := s.currentFilename()
	if filename == "" {
		return fmt.Errorf("gob-snapshot: empty filename")
	}

	// Drain the source first. The gob count header forces this — we can't
	// stream until we know the total. The new v1 format (ADR-0005) uses
	// per-record framing so it can stream end-to-end.
	var entries []SnapshotEntry
	for {
		e, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read snapshot entry: %w", err)
		}
		entries = append(entries, SnapshotEntry{
			Key:        e.Key,
			ValueType:  int(e.ValueType),
			Encoding:   int(e.Encoding),
			Value:      e.Value,
			Expiration: e.Expiration,
		})
	}

	dir := filepath.Dir(filename)
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		logger.Error(ctx).Err(err).Str("file", filename).Msg("failed to create snapshot temp file")
		return fmt.Errorf("create snapshot temp %s: %w", filename, err)
	}
	tmpName := tmp.Name()
	// Clean up the temp file on any failure path; a successful rename
	// makes the cleanup a no-op because the name no longer exists.
	defer func() { _ = os.Remove(tmpName) }()

	encoder := gob.NewEncoder(tmp)
	if err := encoder.Encode(len(entries)); err != nil {
		_ = tmp.Close()
		logger.Error(ctx).Err(err).Str("file", filename).Msg("snapshot encode error")
		return fmt.Errorf("encode snapshot %s: %w", filename, err)
	}
	for _, e := range entries {
		if err := encoder.Encode(e); err != nil {
			_ = tmp.Close()
			logger.Error(ctx).Err(err).Str("file", filename).Msg("snapshot encode error")
			return fmt.Errorf("encode snapshot %s: %w", filename, err)
		}
	}

	// Flush to stable storage before rename. Without Sync, a crash between
	// rename and kernel writeback could still leave a zero-length file at
	// the destination path.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync snapshot %s: %w", filename, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close snapshot %s: %w", filename, err)
	}
	if err := os.Rename(tmpName, filename); err != nil {
		return fmt.Errorf("rename snapshot %s: %w", filename, err)
	}

	logger.Info(ctx).Str("file", filename).Int("entries", len(entries)).Msg("snapshot saved")
	return nil
}

// gobIterator drains a gob-encoded SnapshotEntry stream. It expects exactly
// `remaining` entries — the count header is the contract. Reading more
// returns io.EOF; reading fewer returns an error from gob.
type gobIterator struct {
	file      *os.File
	dec       *gob.Decoder
	remaining int
	closed    atomic.Bool
}

func (it *gobIterator) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	if it.closed.Load() {
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
	if !it.closed.CompareAndSwap(false, true) {
		return nil
	}
	return it.file.Close()
}

// Compile-time assertions: GobSource implements both Source (boot side)
// and Snapshotter (runtime save side).
var _ apipersistence.Source = (*GobSource)(nil)
var _ apipersistence.Snapshotter = (*GobSource)(nil)

// Compile-time assertion: gobIterator implements SnapshotIterator.
var _ apipersistence.SnapshotIterator = (*gobIterator)(nil)

