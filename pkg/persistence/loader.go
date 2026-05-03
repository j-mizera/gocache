package persistence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
	"gocache/pkg/persistence/v1snap"
)

// SnapshotFormat identifies the on-disk format of a snapshot file. The
// values are runtime-internal — a deployment never picks a format
// string from configuration (the active backend is selected at compile
// time via the embedded plugin registration in api/persistence, see
// ADR-0007). DetectFormat exists to support the runtime LOAD_SNAPSHOT
// command, which can read either format regardless of which plugin
// shipped in the binary; that's an operator convenience for loading
// archived snapshots, not a configuration knob.
type SnapshotFormat string

const (
	FormatGob SnapshotFormat = "gob"
	FormatV1  SnapshotFormat = "v1"
)

// DetectFormat sniffs the leading bytes of filename and returns which
// format it is. The v1 format starts with a 4-byte ASCII magic "GCDB"
// (see pkg/persistence/v1snap.format.go); anything else is treated as
// gob, matching the legacy "open and try to gob-decode" semantics.
//
// A missing file returns os.ErrNotExist — callers decide whether that
// is an error or a "nothing to load" signal.
func DetectFormat(filename string) (SnapshotFormat, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf [4]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("v1snap detect: read %s: %w", filename, err)
	}
	// A short file (< 4 bytes) can't be v1 — its header is 5 bytes.
	// Fall through to gob; gob's decoder will surface a clear error
	// on a malformed stream.
	if n == 4 && buf[0] == 'G' && buf[1] == 'C' && buf[2] == 'D' && buf[3] == 'B' {
		return FormatV1, nil
	}
	return FormatGob, nil
}

// LoadFrom reads filename via the format-appropriate Source and applies
// the snapshot to target. Used by the LOAD_SNAPSHOT runtime command —
// distinct from Coordinator.BootInto, which loads through the
// registered snapshot provider. LoadFrom mid-lifecycle CLEARS target
// before loading (matching legacy LOAD_SNAPSHOT semantics).
//
// Format auto-detection means an operator can always read an archived
// gob snapshot file even on a binary that compiled in only the v1
// plugin — DetectFormat picks the right Source based on the bytes on
// disk, not on what's registered.
//
// Errors from the Source bubble up untouched. Truncated / corrupted
// files surface either at Boot (header validation) or on iterator
// drain (CRC mismatch, malformed records).
func LoadFrom(ctx context.Context, filename string, target *cache.Cache) error {
	format, err := DetectFormat(filename)
	if err != nil {
		return fmt.Errorf("detect format: %w", err)
	}

	var src apipersistence.Source
	switch format {
	case FormatV1:
		src = v1snap.NewSource(filename)
	case FormatGob:
		src = NewGobSource(filename)
	default:
		return fmt.Errorf("unsupported snapshot format %q", format)
	}

	boot, err := src.Boot(ctx)
	if err != nil {
		return fmt.Errorf("boot %s: %w", filename, err)
	}
	defer func() { _ = boot.Close() }()

	switch boot.Mode {
	case apipersistence.BootModeInitial:
		// Empty / missing file at this stage is unusual — DetectFormat
		// would have surfaced it. Treat as a successful no-op clear so
		// LOAD_SNAPSHOT against an empty file leaves target empty.
		target.Clear(ctx)
		return nil
	case apipersistence.BootModeSnapshot:
		_, err := loadSnapshotInto(ctx, boot.Snapshot, target)
		return err
	case apipersistence.BootModeReplay:
		// Runtime reload from a replay log isn't supported (replay's
		// LSN-ordered semantics don't match a one-shot user command).
		return fmt.Errorf("LOAD_SNAPSHOT does not support replay-mode sources")
	default:
		return fmt.Errorf("%w: %d", apipersistence.ErrInvalidBootMode, boot.Mode)
	}
}
