//go:build aof

package aof

import (
	"encoding/binary"
	"fmt"
	"io"

	apipersistence "gocache/api/persistence"
)

// File header constants (ADR-0016).
var magic = [6]byte{'G', 'O', 'C', 'A', 'O', 'F'}

const (
	headerVersion = 0x01
	headerSize    = 10 // 6 magic + 1 version + 3 reserved
)

func writeHeader(w io.Writer) error {
	var hdr [headerSize]byte
	copy(hdr[:6], magic[:])
	hdr[6] = headerVersion
	_, err := w.Write(hdr[:])
	return err
}

func readHeader(r io.Reader) error {
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("aof: read header: %w", err)
	}
	if hdr[0] != magic[0] || hdr[1] != magic[1] || hdr[2] != magic[2] ||
		hdr[3] != magic[3] || hdr[4] != magic[4] || hdr[5] != magic[5] {
		return fmt.Errorf("aof: bad magic %q", hdr[:6])
	}
	if hdr[6] != headerVersion {
		return fmt.Errorf("aof: unsupported version %d", hdr[6])
	}
	return nil
}

// encodeRecord writes one mutation record to w: varint body-length
// prefix followed by the body (8-byte LE LSN + varint-delimited op +
// varint arg-count + per-arg varint-delimited bytes).
func encodeRecord(w io.Writer, m apipersistence.Mutation, scratch []byte) ([]byte, error) {
	scratch = scratch[:0]
	scratch = appendLSN(scratch, m.LSN)
	scratch = appendVarBytes(scratch, []byte(m.Op))
	scratch = appendUvarint(scratch, uint64(len(m.Args)))
	for _, arg := range m.Args {
		scratch = appendVarBytes(scratch, arg)
	}

	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(scratch)))
	if _, err := w.Write(lenBuf[:n]); err != nil {
		return scratch, err
	}
	_, err := w.Write(scratch)
	return scratch, err
}

// decodeRecord reads one mutation record from r. Returns io.EOF at end
// of file and io.ErrUnexpectedEOF on a torn/truncated record.
func decodeRecord(r io.ByteReader, fullReader io.Reader) (apipersistence.Mutation, error) {
	bodyLen, err := binary.ReadUvarint(r)
	if err != nil {
		return apipersistence.Mutation{}, err
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(fullReader, body); err != nil {
		return apipersistence.Mutation{}, io.ErrUnexpectedEOF
	}

	if len(body) < 8 {
		return apipersistence.Mutation{}, io.ErrUnexpectedEOF
	}
	lsn := apipersistence.LSN(binary.LittleEndian.Uint64(body[:8]))
	off := 8

	op, n, err := readVarBytes(body[off:])
	if err != nil {
		return apipersistence.Mutation{}, io.ErrUnexpectedEOF
	}
	off += n

	argCount, n := binary.Uvarint(body[off:])
	if n <= 0 {
		return apipersistence.Mutation{}, io.ErrUnexpectedEOF
	}
	off += n

	args := make([][]byte, argCount)
	for i := range args {
		args[i], n, err = readVarBytes(body[off:])
		if err != nil {
			return apipersistence.Mutation{}, io.ErrUnexpectedEOF
		}
		off += n
	}

	var key string
	if len(args) > 0 {
		key = string(args[0])
	}
	return apipersistence.Mutation{
		LSN:  lsn,
		Op:   string(op),
		Key:  key,
		Args: args,
	}, nil
}

// --- varint/LE helpers ---

func appendLSN(dst []byte, lsn apipersistence.LSN) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(lsn))
	return append(dst, buf[:]...)
}

func appendUvarint(dst []byte, v uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	return append(dst, buf[:n]...)
}

func appendVarBytes(dst, data []byte) []byte {
	dst = appendUvarint(dst, uint64(len(data)))
	return append(dst, data...)
}

func readVarBytes(buf []byte) ([]byte, int, error) {
	vlen, n := binary.Uvarint(buf)
	if n <= 0 {
		return nil, 0, io.ErrUnexpectedEOF
	}
	end := n + int(vlen)
	if end > len(buf) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	return buf[n:end], end, nil
}
