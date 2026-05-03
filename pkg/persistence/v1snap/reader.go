package v1snap

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"

	apipersistence "gocache/api/persistence"
)

// Source reads the v1 snapshot format. Implements api/persistence.Source
// with the same Boot semantics as the gob shim:
//
//   - missing file → BootModeInitial
//   - empty file (zero bytes) → BootModeInitial
//   - well-formed file with header → BootModeSnapshot, with iterator
//
// Filename is hot-reloadable via SetFilename for parity with the gob
// shim. Boot reads it once at the start; iterator owns the file handle.
type Source struct {
	mu       sync.RWMutex
	filename string
}

// NewSource returns a Source reading from filename.
func NewSource(filename string) *Source {
	return &Source{filename: filename}
}

// Name implements api/persistence.Source.
func (s *Source) Name() string { return "v1-snapshot" }

// SetFilename swaps the on-disk path. Only observable on the next
// Boot call (Boot is one-shot per server lifetime, so this is mostly
// useful for tests and config reload races).
func (s *Source) SetFilename(f string) {
	s.mu.Lock()
	s.filename = f
	s.mu.Unlock()
}

func (s *Source) currentFilename() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.filename
}

// Boot implements api/persistence.Source.
func (s *Source) Boot(_ context.Context) (apipersistence.BootResult, error) {
	filename := s.currentFilename()
	file, err := os.Open(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
		}
		return apipersistence.BootResult{}, fmt.Errorf("v1snap: open %s: %w", filename, err)
	}

	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return apipersistence.BootResult{}, fmt.Errorf("v1snap: stat %s: %w", filename, err)
	}
	if stat.Size() == 0 {
		_ = file.Close()
		return apipersistence.BootResult{Mode: apipersistence.BootModeInitial}, nil
	}
	if stat.Size() < int64(headerLen+footerLen) {
		_ = file.Close()
		return apipersistence.BootResult{}, fmt.Errorf("v1snap: %s too small (%d bytes)", filename, stat.Size())
	}

	// Read and validate header.
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(file, hdr); err != nil {
		_ = file.Close()
		return apipersistence.BootResult{}, fmt.Errorf("v1snap: read header %s: %w", filename, err)
	}
	if hdr[0] != magic0 || hdr[1] != magic1 || hdr[2] != magic2 || hdr[3] != magic3 {
		_ = file.Close()
		return apipersistence.BootResult{}, fmt.Errorf("v1snap: bad magic in %s", filename)
	}
	if hdr[4] != formatVersion {
		_ = file.Close()
		return apipersistence.BootResult{}, fmt.Errorf("v1snap: unsupported format version %d in %s (this build expects %d)", hdr[4], filename, formatVersion)
	}

	// Body extends from the end of the header to the start of the
	// footer. The iterator reads bytes from a bounded reader so the
	// CRC validation on Close knows it has consumed the right span.
	bodyLen := stat.Size() - int64(headerLen) - int64(footerLen)
	body := io.LimitReader(file, bodyLen)

	hasher := crc32.NewIEEE()
	hasher.Write(hdr) // header bytes are part of the CRC scope

	dec, err := zstd.NewReader(nil)
	if err != nil {
		_ = file.Close()
		return apipersistence.BootResult{}, fmt.Errorf("v1snap: zstd reader init: %w", err)
	}

	it := &iterator{
		file:    file,
		body:    bufio.NewReader(io.TeeReader(body, hasher)),
		hasher:  hasher,
		decoder: dec,
		bodyLen: bodyLen,
	}

	// Peek the first record. If it's META(LSN), consume it and pin the
	// LSN in the BootResult so the Coordinator seeds its cursor.
	var lsn apipersistence.LSN
	consumed, lsnVal, err := it.peekMetaLSN()
	if err != nil {
		it.Close()
		return apipersistence.BootResult{}, fmt.Errorf("v1snap: META: %w", err)
	}
	if consumed {
		lsn = lsnVal
	}

	return apipersistence.BootResult{
		Mode:     apipersistence.BootModeSnapshot,
		LSN:      lsn,
		Snapshot: it,
	}, nil
}

// iterator is the concrete SnapshotIterator. It streams records from
// the body (length-prefixed framing) until exhaustion or a length
// mismatch. Close validates the CRC32 footer.
type iterator struct {
	file    *os.File
	body    *bufio.Reader
	hasher  hashWriter32
	decoder *zstd.Decoder
	bodyLen int64
	read    int64 // bytes consumed from body
	closed  bool
	// scratch reduces per-record allocations on the read path. The
	// returned SnapshotEntry copies its key and value out of scratch
	// before iterator state advances, so callers never alias it.
	scratch []byte
	// pending holds an entry the iterator already parsed (e.g., when
	// peekMetaLSN consumed the first record looking for META and
	// found a data record instead). The next Next() call drains it.
	pending *apipersistence.SnapshotEntry
}

// hashWriter32 is the subset of hash.Hash32 the iterator needs. Avoids
// importing hash from packages that only need the contract.
type hashWriter32 interface {
	io.Writer
	Sum32() uint32
}

// Next implements api/persistence.SnapshotIterator.
func (it *iterator) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	if it.closed {
		return apipersistence.SnapshotEntry{}, io.EOF
	}
	if it.pending != nil {
		e := *it.pending
		it.pending = nil
		return e, nil
	}
	if it.read >= it.bodyLen {
		return apipersistence.SnapshotEntry{}, io.EOF
	}

	bodyLen, err := readUvarintFromReader(it.body)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return apipersistence.SnapshotEntry{}, io.EOF
		}
		return apipersistence.SnapshotEntry{}, fmt.Errorf("read record length: %w", err)
	}
	it.read += int64(uvarintLen(bodyLen))

	if int64(bodyLen) > it.bodyLen-it.read {
		return apipersistence.SnapshotEntry{}, fmt.Errorf("record length %d exceeds remaining body %d", bodyLen, it.bodyLen-it.read)
	}

	if cap(it.scratch) < int(bodyLen) {
		it.scratch = make([]byte, bodyLen)
	} else {
		it.scratch = it.scratch[:bodyLen]
	}
	if _, err := io.ReadFull(it.body, it.scratch); err != nil {
		return apipersistence.SnapshotEntry{}, fmt.Errorf("read record body: %w", err)
	}
	it.read += int64(bodyLen)

	return parseDataRecord(it.scratch, it.decoder)
}

// peekMetaLSN consumes a leading META(LSN) record if present. Returns
// (true, lsn, nil) when consumed, (false, 0, nil) when the first
// record is a data record, or an error on a malformed META.
func (it *iterator) peekMetaLSN() (bool, apipersistence.LSN, error) {
	if it.read >= it.bodyLen {
		return false, 0, nil
	}
	bodyLen, err := readUvarintFromReader(it.body)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("META length: %w", err)
	}
	it.read += int64(uvarintLen(bodyLen))

	if int64(bodyLen) > it.bodyLen-it.read {
		return false, 0, fmt.Errorf("META length %d exceeds remaining body", bodyLen)
	}

	if cap(it.scratch) < int(bodyLen) {
		it.scratch = make([]byte, bodyLen)
	} else {
		it.scratch = it.scratch[:bodyLen]
	}
	if _, err := io.ReadFull(it.body, it.scratch); err != nil {
		return false, 0, fmt.Errorf("META body: %w", err)
	}
	it.read += int64(bodyLen)

	if len(it.scratch) < 1 {
		return false, 0, fmt.Errorf("empty record body")
	}
	if it.scratch[0] != typeMeta {
		// Not a META record — we already consumed it from the stream.
		// Re-parse it as a data record so the caller doesn't lose it.
		// Implementation choice: meta is always first if present, so
		// the second-best path is to fall through with a synthetic
		// rewind. Simpler: deliver this record as the first Next() —
		// stash it on the iterator.
		entry, parseErr := parseDataRecord(it.scratch, it.decoder)
		if parseErr != nil {
			return false, 0, parseErr
		}
		it.pending = &entry
		return false, 0, nil
	}

	// META record.
	if len(it.scratch) < 2 || it.scratch[1] != metaSubLSN {
		// Unknown sub-tag — skip silently per format.go forward-compat
		// rule. The outer length already advanced our cursor.
		return false, 0, nil
	}
	lsn, _, err := readUvarint(it.scratch[2:])
	if err != nil {
		return false, 0, fmt.Errorf("META LSN payload: %w", err)
	}
	return true, apipersistence.LSN(lsn), nil
}

// Close implements api/persistence.SnapshotIterator.
func (it *iterator) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	if it.decoder != nil {
		it.decoder.Close()
	}

	// Read and validate the footer regardless of whether the iterator
	// was drained — a partial read with a checksum mismatch should still
	// surface as an error.
	footer := make([]byte, footerLen)
	if _, err := io.ReadFull(it.file, footer); err != nil {
		_ = it.file.Close()
		return fmt.Errorf("v1snap: read footer: %w", err)
	}
	got := binary.LittleEndian.Uint32(footer)
	want := it.hasher.Sum32()
	if got != want {
		_ = it.file.Close()
		return fmt.Errorf("v1snap: CRC mismatch (file=%08x computed=%08x)", got, want)
	}
	return it.file.Close()
}

// parseDataRecord decodes a record body into a SnapshotEntry. Inverse
// of writeDataRecord in writer.go.
func parseDataRecord(body []byte, dec *zstd.Decoder) (apipersistence.SnapshotEntry, error) {
	if len(body) < 3 {
		return apipersistence.SnapshotEntry{}, fmt.Errorf("record body too short (%d)", len(body))
	}
	typeTag := body[0]
	flags := body[1]
	encoding := body[2]
	rest := body[3:]

	vt, err := tagToValueType(typeTag)
	if err != nil {
		return apipersistence.SnapshotEntry{}, err
	}

	// Expiration (zigzag varint).
	expir, n := binary.Varint(rest)
	if n <= 0 {
		return apipersistence.SnapshotEntry{}, fmt.Errorf("expiration varint malformed")
	}
	rest = rest[n:]

	keyLen, n, err := readUvarint(rest)
	if err != nil {
		return apipersistence.SnapshotEntry{}, fmt.Errorf("key length: %w", err)
	}
	rest = rest[n:]
	if uint64(len(rest)) < keyLen {
		return apipersistence.SnapshotEntry{}, fmt.Errorf("key short read")
	}
	key := string(rest[:keyLen])
	rest = rest[keyLen:]

	valLen, n, err := readUvarint(rest)
	if err != nil {
		return apipersistence.SnapshotEntry{}, fmt.Errorf("value length: %w", err)
	}
	rest = rest[n:]
	if uint64(len(rest)) < valLen {
		return apipersistence.SnapshotEntry{}, fmt.Errorf("value short read")
	}
	valBlob := rest[:valLen]

	if flags&flagZstd != 0 {
		decoded, err := dec.DecodeAll(valBlob, nil)
		if err != nil {
			return apipersistence.SnapshotEntry{}, fmt.Errorf("zstd decode: %w", err)
		}
		valBlob = decoded
	} else {
		// Copy out of the iterator's scratch so the entry doesn't
		// alias a buffer that the next Next() will overwrite. The
		// compressed path already returns a fresh slice.
		cp := make([]byte, len(valBlob))
		copy(cp, valBlob)
		valBlob = cp
	}

	enc, err := tagToEncoding(encoding)
	if err != nil {
		return apipersistence.SnapshotEntry{}, err
	}
	var value any
	switch enc {
	case apipersistence.EncodingPacked:
		value = valBlob
	case apipersistence.EncodingNative:
		v, err := decodeNativeValue(vt, valBlob)
		if err != nil {
			return apipersistence.SnapshotEntry{}, fmt.Errorf("native decode: %w", err)
		}
		value = v
	default:
		return apipersistence.SnapshotEntry{}, fmt.Errorf("unknown Encoding %d", encoding)
	}

	return apipersistence.SnapshotEntry{
		Key:        key,
		ValueType:  vt,
		Encoding:   enc,
		Value:      value,
		Expiration: expir,
	}, nil
}

// tagToEncoding is the inverse of encodingTag. Unknown bytes are an
// error so a forward-compatible writer can't silently corrupt our
// reader by introducing a third encoding.
func tagToEncoding(b byte) (apipersistence.Encoding, error) {
	switch b {
	case encNative:
		return apipersistence.EncodingNative, nil
	case encPacked:
		return apipersistence.EncodingPacked, nil
	default:
		return 0, fmt.Errorf("unknown encoding byte %#x", b)
	}
}

// tagToValueType is the inverse of valueTypeTag. Unknown tags return
// an error rather than panicking — bad bytes shouldn't crash the
// server during boot.
func tagToValueType(tag byte) (apipersistence.ValueType, error) {
	switch tag {
	case typeString:
		return apipersistence.ValueTypeBytes, nil
	case typeList:
		return apipersistence.ValueTypeList, nil
	case typeHash:
		return apipersistence.ValueTypeHash, nil
	case typeSet:
		return apipersistence.ValueTypeSet, nil
	case typeZSet:
		return apipersistence.ValueTypeSortedSet, nil
	default:
		return 0, fmt.Errorf("unknown type tag %#x", tag)
	}
}

// readUvarintFromReader reads a varint from a bufio.Reader. binary.Uvarint
// requires a []byte; binary.ReadUvarint requires io.ByteReader. bufio
// satisfies both, but ReadUvarint is the natural fit here.
func readUvarintFromReader(r *bufio.Reader) (uint64, error) {
	v, err := binary.ReadUvarint(r)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// uvarintLen returns the byte width of v's varint encoding. Used by
// the iterator to advance its read counter alongside the body bytes.
func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// Compile-time assertions.
var _ apipersistence.Source = (*Source)(nil)
var _ apipersistence.SnapshotIterator = (*iterator)(nil)
