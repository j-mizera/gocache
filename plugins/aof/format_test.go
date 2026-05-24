//go:build aof

package aof

import (
	"bytes"
	"io"
	"testing"

	apipersistence "gocache/api/persistence"
)

func TestHeaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeHeader(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != headerSize {
		t.Fatalf("header size = %d, want %d", buf.Len(), headerSize)
	}
	if err := readHeader(&buf); err != nil {
		t.Fatalf("readHeader: %v", err)
	}
}

func TestHeaderBadMagic(t *testing.T) {
	hdr := make([]byte, headerSize)
	copy(hdr, "BADMAG")
	if err := readHeader(bytes.NewReader(hdr)); err == nil {
		t.Error("expected error on bad magic")
	}
}

func TestHeaderBadVersion(t *testing.T) {
	var buf bytes.Buffer
	writeHeader(&buf)
	data := buf.Bytes()
	data[6] = 0xFF
	if err := readHeader(bytes.NewReader(data)); err == nil {
		t.Error("expected error on bad version")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	mutations := []apipersistence.Mutation{
		{LSN: 1, Op: "SET", Key: "k", Args: [][]byte{[]byte("k"), []byte("v")}},
		{LSN: 2, Op: "HSET", Key: "h", Args: [][]byte{[]byte("h"), []byte("f"), []byte("v")}},
		{LSN: 3, Op: "DEL", Key: "x", Args: [][]byte{[]byte("x")}},
		{LSN: 4, Op: "ZADD", Key: "z", Args: [][]byte{[]byte("z"), []byte("1.5"), []byte("m")}},
	}

	var buf bytes.Buffer
	scratch := make([]byte, 0, 256)
	for _, m := range mutations {
		var err error
		scratch, err = encodeRecord(&buf, m, scratch)
		if err != nil {
			t.Fatalf("encode %s: %v", m.Op, err)
		}
	}

	r := bytes.NewReader(buf.Bytes())
	for i, want := range mutations {
		got, err := decodeRecord(r, r)
		if err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if got.LSN != want.LSN {
			t.Errorf("[%d] LSN = %d, want %d", i, got.LSN, want.LSN)
		}
		if got.Op != want.Op {
			t.Errorf("[%d] Op = %q, want %q", i, got.Op, want.Op)
		}
		if len(got.Args) != len(want.Args) {
			t.Errorf("[%d] args len = %d, want %d", i, len(got.Args), len(want.Args))
			continue
		}
		for j := range want.Args {
			if !bytes.Equal(got.Args[j], want.Args[j]) {
				t.Errorf("[%d] arg[%d] = %q, want %q", i, j, got.Args[j], want.Args[j])
			}
		}
	}

	_, err := decodeRecord(r, r)
	if err != io.EOF {
		t.Errorf("expected io.EOF after last record, got %v", err)
	}
}

func TestDecodeRecord_TornWrite(t *testing.T) {
	m := apipersistence.Mutation{LSN: 42, Op: "SET", Args: [][]byte{[]byte("k"), []byte("v")}}
	var buf bytes.Buffer
	encodeRecord(&buf, m, nil)

	full := buf.Bytes()
	truncated := full[:len(full)-3]

	r := bytes.NewReader(truncated)
	_, err := decodeRecord(r, r)
	if err != io.ErrUnexpectedEOF {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestDecodeRecord_EmptyBody(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0x02)
	buf.Write([]byte{0, 0})
	r := bytes.NewReader(buf.Bytes())
	_, err := decodeRecord(r, r)
	if err != io.ErrUnexpectedEOF {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}
