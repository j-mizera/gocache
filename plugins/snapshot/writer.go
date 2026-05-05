package snapshot

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/klauspost/compress/zstd"

	"gocache/api/logger"
	apipersistence "gocache/api/persistence"
)

// Snapshotter writes the v1 snapshot format to a file path. Implements
// api/persistence.Snapshotter. SaveSnapshot streams records as it pulls
// them from the SnapshotSource — there is no count header, so memory
// stays bounded by the largest single record (varint length + value
// blob), not by the total snapshot size.
//
// Write path:
//  1. Create a sibling temp file in the same directory as filename.
//  2. Buffer-wrap the file; write header (magic + version).
//  3. For each record from src.Next: encode body, optionally zstd-
//     compress the value blob, write `varint(len)` + body.
//  4. Append CRC32 footer over header+records.
//  5. Sync, close, rename atomically over filename.
//
// Crash mid-write leaves the previous snapshot intact (the temp file
// gets cleaned up on the failure path; rename is atomic at the kernel
// level on POSIX).
//
// Filename is hot-reloadable via SetFilename (mutex-guarded so config
// reload races safely with an in-flight save).
type Snapshotter struct {
	mu       sync.RWMutex
	filename string

	// LSN, when non-zero, is written as a META(LSN) record at the start
	// of the stream. Set via SetLSN before SaveSnapshot. When zero, no
	// META record is emitted — the gob shim has no LSN concept and a
	// roundtrip must produce a v1 file that the v1 reader treats as
	// LSN=0 (no resume cursor).
	lsnMu sync.RWMutex
	lsn   apipersistence.LSN
}

// NewSnapshotter returns a writer targeting filename. Filename is
// resolved at SaveSnapshot time, not now — config hot-reload may
// rewrite it via SetFilename between construction and use.
func NewSnapshotter(filename string) *Snapshotter {
	return &Snapshotter{filename: filename}
}

// Name implements api/persistence.Snapshotter.
func (s *Snapshotter) Name() string { return "snapshot" }

// SetFilename swaps the on-disk path. Concurrent with in-flight
// SaveSnapshot — the path-read inside SaveSnapshot grabs an RLock so
// the change is observed atomically at save start (not mid-save).
func (s *Snapshotter) SetFilename(f string) {
	s.mu.Lock()
	s.filename = f
	s.mu.Unlock()
}

// SetLSN seeds the META(LSN) record emitted at the start of the next
// SaveSnapshot. Pass ZeroLSN to suppress the META record.
func (s *Snapshotter) SetLSN(lsn apipersistence.LSN) {
	s.lsnMu.Lock()
	s.lsn = lsn
	s.lsnMu.Unlock()
}

func (s *Snapshotter) currentFilename() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.filename
}

func (s *Snapshotter) currentLSN() apipersistence.LSN {
	s.lsnMu.RLock()
	defer s.lsnMu.RUnlock()
	return s.lsn
}

// SaveSnapshot implements api/persistence.Snapshotter.
func (s *Snapshotter) SaveSnapshot(ctx context.Context, src apipersistence.SnapshotSource) error {
	filename := s.currentFilename()
	if filename == "" {
		return errors.New("snapshot: empty filename")
	}

	dir := filepath.Dir(filename)
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("snapshot: create temp %s: %w", filename, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on every failure path; a successful rename
	// makes Remove a no-op because the name no longer exists.
	defer func() { _ = os.Remove(tmpName) }()

	bw := bufio.NewWriter(tmp)
	hasher := crc32.NewIEEE()
	w := io.MultiWriter(bw, hasher)

	if err := writeHeader(w); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("snapshot: write header: %w", err)
	}

	// Optional META(LSN) record up front.
	if lsn := s.currentLSN(); lsn != apipersistence.ZeroLSN {
		if err := writeMetaLSN(w, lsn); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("snapshot: write META(LSN): %w", err)
		}
	}

	// Get a zstd encoder once and reuse across records — Reset on the
	// next record avoids re-allocating compression state per entry.
	enc, err := zstd.NewWriter(nil) // nil writer; we use EncodeAll
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("snapshot: zstd encoder init: %w", err)
	}
	defer enc.Close()

	count := 0
	for {
		e, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = tmp.Close()
			return fmt.Errorf("snapshot: read entry %d: %w", count, err)
		}
		if err := writeDataRecord(w, e, enc); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("snapshot: write entry %d (key=%q): %w", count, e.Key, err)
		}
		count++
	}

	// Footer: CRC32 of everything written so far. Write goes to bw
	// only — we don't fold the CRC bytes back into the hasher.
	if err := bw.Flush(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("snapshot: flush: %w", err)
	}
	footer := make([]byte, footerLen)
	binary.LittleEndian.PutUint32(footer, hasher.Sum32())
	if _, err := tmp.Write(footer); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("snapshot: write footer: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("snapshot: sync %s: %w", filename, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: close %s: %w", filename, err)
	}
	if err := os.Rename(tmpName, filename); err != nil {
		return fmt.Errorf("snapshot: rename %s: %w", filename, err)
	}

	logger.Info(ctx).Str("file", filename).Int("entries", count).Msg("snapshot: snapshot saved")
	return nil
}

// writeHeader emits the 5-byte fixed prefix.
func writeHeader(w io.Writer) error {
	hdr := [headerLen]byte{magic0, magic1, magic2, magic3, formatVersion}
	_, err := w.Write(hdr[:])
	return err
}

// writeMetaLSN emits one META record carrying the LSN cursor.
//
//	body = [typeMeta][metaSubLSN][varint LSN]
func writeMetaLSN(w io.Writer, lsn apipersistence.LSN) error {
	body := make([]byte, 0, 2+binary.MaxVarintLen64)
	body = append(body, typeMeta, metaSubLSN)
	body = binary.AppendUvarint(body, uint64(lsn))
	return writeLengthPrefixed(w, body)
}

// writeDataRecord encodes and emits one cache entry as a v1 data
// record. Compression is decided per-record based on the encoded value
// blob's size and the post-compression savings.
func writeDataRecord(w io.Writer, e apipersistence.SnapshotEntry, enc *zstd.Encoder) error {
	var (
		valueBlob []byte
		err       error
	)
	switch e.Encoding {
	case apipersistence.EncodingPacked:
		// Packed entries store the slab buffer verbatim.
		switch v := e.Value.(type) {
		case []byte:
			valueBlob = v
		case string:
			valueBlob = []byte(v)
		default:
			return fmt.Errorf("packed value: want []byte/string, got %T", e.Value)
		}
	case apipersistence.EncodingNative:
		valueBlob, err = encodeNativeValue(e.ValueType, e.Value)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown Encoding %d", e.Encoding)
	}

	// Compression decision.
	var (
		compressed []byte
		flags      byte
	)
	if len(valueBlob) >= compressionThreshold {
		compressed = enc.EncodeAll(valueBlob, nil)
		if len(valueBlob)-len(compressed) >= minCompressionGain {
			valueBlob = compressed
			flags |= flagZstd
		}
	}

	// Body assembly:
	//   [type tag][flags][encoding][varint expir][varint key-len][key][varint val-len][val]
	encodingByte, err := encodingTag(e.Encoding)
	if err != nil {
		return err
	}
	body := make([]byte, 0, 4+binary.MaxVarintLen64*3+len(e.Key)+len(valueBlob))
	body = append(body, valueTypeTag(e.ValueType), flags, encodingByte)
	body = binary.AppendVarint(body, e.Expiration)
	body = binary.AppendUvarint(body, uint64(len(e.Key)))
	body = append(body, e.Key...)
	body = binary.AppendUvarint(body, uint64(len(valueBlob)))
	body = append(body, valueBlob...)

	return writeLengthPrefixed(w, body)
}

// writeLengthPrefixed writes a record body prefixed with its varint
// byte length. The reader uses the length to skip records of unknown
// type without parsing the body.
func writeLengthPrefixed(w io.Writer, body []byte) error {
	prefix := make([]byte, 0, binary.MaxVarintLen64)
	prefix = binary.AppendUvarint(prefix, uint64(len(body)))
	if _, err := w.Write(prefix); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// encodingTag maps an api/persistence.Encoding to the wire-format
// encoding byte. The mapping is explicit (rather than a numeric cast)
// so the wire format stays decoupled from the api enum's numeric
// values — adding a third api Encoding doesn't silently change the
// wire format.
func encodingTag(enc apipersistence.Encoding) (byte, error) {
	switch enc {
	case apipersistence.EncodingNative:
		return encNative, nil
	case apipersistence.EncodingPacked:
		return encPacked, nil
	default:
		return 0, fmt.Errorf("snapshot: unknown Encoding %d", enc)
	}
}

// valueTypeTag maps an api/persistence.ValueType to the wire-format
// type tag. Unknown types panic — they should have been caught earlier
// by encodeNativeValue. Zero-tag (TypeMeta) is reserved and never
// emitted from this path.
func valueTypeTag(vt apipersistence.ValueType) byte {
	switch vt {
	case apipersistence.ValueTypeBytes:
		return typeString
	case apipersistence.ValueTypeList:
		return typeList
	case apipersistence.ValueTypeHash:
		return typeHash
	case apipersistence.ValueTypeSet:
		return typeSet
	case apipersistence.ValueTypeSortedSet:
		return typeZSet
	default:
		panic(fmt.Sprintf("snapshot: unknown ValueType %d", vt))
	}
}

// Compile-time assertions: Snapshotter implements the api contract,
// and the optional LSNSeeder capability so the coordinator seeds the
// cursor before each save (ADR-0005 META record).
var _ apipersistence.Snapshotter = (*Snapshotter)(nil)
var _ apipersistence.LSNSeeder = (*Snapshotter)(nil)
