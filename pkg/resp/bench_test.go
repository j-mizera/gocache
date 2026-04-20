package resp

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkWrite_BulkString simulates a single GET response.
func BenchmarkWrite_BulkString(b *testing.B) {
	w := NewWriter(io.Discard)
	v := MarshalBulkString("hello world")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.Write(v)
	}
	_ = w.Flush()
}

// BenchmarkWrite_ArraySmall simulates an LRANGE / MGET response with 10 elements.
func BenchmarkWrite_ArraySmall(b *testing.B) {
	w := NewWriter(io.Discard)
	elems := make([]Value, 10)
	for i := range elems {
		elems[i] = MarshalBulkString("item" + strconv.Itoa(i))
	}
	v := Value{Type: Array, Array: elems}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.Write(v)
	}
	_ = w.Flush()
}

// BenchmarkWrite_ArrayLarge simulates a large LRANGE with 1000 items. Exercises
// the recursive marshalValue append hot path.
func BenchmarkWrite_ArrayLarge(b *testing.B) {
	w := NewWriter(io.Discard)
	elems := make([]Value, 1000)
	for i := range elems {
		elems[i] = MarshalBulkString("item" + strconv.Itoa(i))
	}
	v := Value{Type: Array, Array: elems}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.Write(v)
	}
	_ = w.Flush()
}

// BenchmarkWrite_Map simulates HGETALL with 20 field/value pairs (Map = 40 entries).
func BenchmarkWrite_Map(b *testing.B) {
	w := NewWriter(io.Discard)
	pairs := make([]Value, 40)
	for i := 0; i < 20; i++ {
		pairs[2*i] = MarshalBulkString("field" + strconv.Itoa(i))
		pairs[2*i+1] = MarshalBulkString("value" + strconv.Itoa(i))
	}
	v := Value{Type: Map, Array: pairs}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.Write(v)
	}
	_ = w.Flush()
}

// BenchmarkWrite_Pipelined simulates 100 GET responses in a single batch — this
// is what matters most on a busy server where replies coalesce into one syscall.
func BenchmarkWrite_Pipelined(b *testing.B) {
	w := NewWriter(io.Discard)
	v := MarshalBulkString("hello world")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 100; j++ {
			_ = w.Write(v)
		}
		_ = w.Flush()
	}
}

// buildBulkStringPayload returns "$len\r\n<data>\r\n" for a single bulk string.
func buildBulkStringPayload(s string) []byte {
	var b bytes.Buffer
	b.WriteByte(BulkString)
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteString("\r\n")
	b.WriteString(s)
	b.WriteString("\r\n")
	return b.Bytes()
}

// buildArrayPayload returns a RESP2 array of bulk strings.
func buildArrayPayload(items []string) []byte {
	var b bytes.Buffer
	b.WriteByte(Array)
	b.WriteString(strconv.Itoa(len(items)))
	b.WriteString("\r\n")
	for _, s := range items {
		b.WriteByte(BulkString)
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteString("\r\n")
		b.WriteString(s)
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// multiReader repeats a fixed payload indefinitely. Lets Read benchmarks hoist
// Reader construction out of the timed loop while still giving each iteration
// fresh bytes to parse — otherwise the bufio.Reader would hit EOF after the
// first parse.
type multiReader struct {
	payload []byte
	pos     int
}

func (m *multiReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if m.pos >= len(m.payload) {
			m.pos = 0
		}
		c := copy(p[n:], m.payload[m.pos:])
		m.pos += c
		n += c
	}
	return n, nil
}

// BenchmarkRead_BulkString parses a bulk string (e.g. SET value payload).
func BenchmarkRead_BulkString(b *testing.B) {
	payload := buildBulkStringPayload(strings.Repeat("x", 64))
	r := NewReader(&multiReader{payload: payload})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Read(); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkRead_Array parses a 10-element array (e.g. LPUSH key v1..v10).
func BenchmarkRead_Array(b *testing.B) {
	items := make([]string, 10)
	for i := range items {
		items[i] = "item" + strconv.Itoa(i)
	}
	payload := buildArrayPayload(items)
	r := NewReader(&multiReader{payload: payload})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Read(); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkRead_ArrayLarge parses a 1000-element array.
func BenchmarkRead_ArrayLarge(b *testing.B) {
	items := make([]string, 1000)
	for i := range items {
		items[i] = "item" + strconv.Itoa(i)
	}
	payload := buildArrayPayload(items)
	r := NewReader(&multiReader{payload: payload})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Read(); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}
