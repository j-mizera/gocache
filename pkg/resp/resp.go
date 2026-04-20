package resp

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// RESP2 types
	SimpleString = '+'
	Error        = '-'
	Integer      = ':'
	BulkString   = '$'
	Array        = '*'

	// RESP3 types
	Null      = '_'
	Boolean   = '#'
	Double    = ','
	BulkError = '!'
	Map       = '%'
	Set       = '~'
)

// Resource limits to reject malformed or malicious input before allocation.
const (
	// maxBulkStringBytes caps the size of a single bulk string.
	// Prevents memory exhaustion via `$<huge>\r\n`.
	maxBulkStringBytes = 512 * 1024 * 1024 // 512 MiB

	// maxArrayElements caps the number of elements in an Array/Map/Set.
	// Prevents memory exhaustion via `*<huge>\r\n` and downstream makes.
	maxArrayElements = 1024 * 1024 // 1 M elements

	// defaultWriterBufSize is the bufio.Writer buffer size used to batch
	// small RESP writes (pipelined replies) into larger syscalls.
	defaultWriterBufSize = 16 * 1024
)

type Value struct {
	Type    byte
	Str     string
	Integer int
	Array   []Value
	IsNull  bool
	Float64 float64
	Bool    bool
}

type Reader struct {
	reader *bufio.Reader
}

func NewReader(rd io.Reader) *Reader {
	return &Reader{
		reader: bufio.NewReader(rd),
	}
}

func (r *Reader) ReadLine() (line []byte, err error) {
	b, err := r.reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(b) >= 2 && b[len(b)-2] == '\r' {
		return b[:len(b)-2], nil
	}
	// Some clients might not send \r
	return b[:len(b)-1], nil
}

func (r *Reader) Buffered() int {
	return r.reader.Buffered()
}

func (r *Reader) readInteger() (x int, n int, err error) {
	line, err := r.ReadLine()
	if err != nil {
		return 0, 0, err
	}
	i64, err := strconv.ParseInt(string(line), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	return int(i64), len(line), nil
}

func (r *Reader) Read() (Value, error) {
	typeByte, err := r.reader.ReadByte()
	if err != nil {
		return Value{}, err
	}

	switch typeByte {
	case Array:
		return r.readArray()
	case BulkString:
		return r.readBulkString()
	case SimpleString:
		return r.readSimpleString()
	case Error:
		return r.readError()
	case Integer:
		return r.readInt()
	case Null:
		return r.readNull()
	case Boolean:
		return r.readBoolean()
	case Double:
		return r.readDouble()
	case BulkError:
		return r.readBulkError()
	case Map:
		return r.readMap()
	case Set:
		return r.readSet()
	default:
		return r.readInlineCommand(typeByte)
	}
}

// readInlineCommand handles non-RESP input (e.g. "PING\r\n" from telnet).
// The first byte has already been consumed, so we unread it and read the line.
func (r *Reader) readInlineCommand(firstByte byte) (Value, error) {
	if err := r.reader.UnreadByte(); err != nil {
		return Value{}, err
	}
	line, err := r.ReadLine()
	if err != nil {
		return Value{}, err
	}
	parts := strings.Fields(string(line))
	if len(parts) == 0 {
		return Value{}, fmt.Errorf("empty inline command")
	}
	v := Value{Type: Array, Array: make([]Value, len(parts))}
	for i, p := range parts {
		v.Array[i] = Value{Type: BulkString, Str: p}
	}
	return v, nil
}

func (r *Reader) readArray() (Value, error) {
	v := Value{Type: Array}
	n, _, err := r.readInteger()
	if err != nil {
		return v, err
	}

	// Null array (RESP2): *-1\r\n
	if n < 0 {
		v.IsNull = true
		return v, nil
	}
	if n > maxArrayElements {
		return v, fmt.Errorf("resp: array too large: %d (max %d)", n, maxArrayElements)
	}

	v.Array = make([]Value, n)
	for i := 0; i < n; i++ {
		val, err := r.Read()
		if err != nil {
			return v, err
		}
		v.Array[i] = val
	}
	return v, nil
}

func (r *Reader) readBulkString() (Value, error) {
	v := Value{Type: BulkString}
	n, _, err := r.readInteger()
	if err != nil {
		return v, err
	}

	if n == -1 {
		v.IsNull = true
		return v, nil // Null Bulk String
	}
	if n < 0 {
		return v, fmt.Errorf("resp: invalid bulk string length: %d", n)
	}
	if n > maxBulkStringBytes {
		return v, fmt.Errorf("resp: bulk string too large: %d (max %d)", n, maxBulkStringBytes)
	}

	bufp := getBulkScratch(n)
	if _, err := io.ReadFull(r.reader, *bufp); err != nil {
		putBulkScratch(bufp)
		return v, err
	}

	// Read CRLF
	if _, err := r.ReadLine(); err != nil {
		putBulkScratch(bufp)
		return v, err
	}

	v.Str = string(*bufp)
	putBulkScratch(bufp)
	return v, nil
}

func (r *Reader) readSimpleString() (Value, error) {
	line, err := r.ReadLine()
	if err != nil {
		return Value{}, err
	}
	return Value{Type: SimpleString, Str: string(line)}, nil
}

func (r *Reader) readError() (Value, error) {
	line, err := r.ReadLine()
	if err != nil {
		return Value{}, err
	}
	return Value{Type: Error, Str: string(line)}, nil
}

func (r *Reader) readInt() (Value, error) {
	i, _, err := r.readInteger()
	if err != nil {
		return Value{}, err
	}
	return Value{Type: Integer, Integer: i}, nil
}

func (r *Reader) readNull() (Value, error) {
	if _, err := r.ReadLine(); err != nil {
		return Value{}, err
	}
	return Value{Type: Null, IsNull: true}, nil
}

func (r *Reader) readBoolean() (Value, error) {
	line, err := r.ReadLine()
	if err != nil {
		return Value{}, err
	}
	if len(line) != 1 || (line[0] != 't' && line[0] != 'f') {
		return Value{}, fmt.Errorf("invalid boolean value: %q", string(line))
	}
	return Value{Type: Boolean, Bool: line[0] == 't'}, nil
}

func (r *Reader) readDouble() (Value, error) {
	line, err := r.ReadLine()
	if err != nil {
		return Value{}, err
	}
	f, err := strconv.ParseFloat(string(line), 64)
	if err != nil {
		return Value{}, fmt.Errorf("invalid double value: %w", err)
	}
	return Value{Type: Double, Float64: f}, nil
}

func (r *Reader) readBulkError() (Value, error) {
	v := Value{Type: BulkError}
	length, _, err := r.readInteger()
	if err != nil {
		return v, err
	}
	if length < 0 {
		return v, fmt.Errorf("resp: invalid bulk error length: %d", length)
	}
	if length > maxBulkStringBytes {
		return v, fmt.Errorf("resp: bulk error too large: %d (max %d)", length, maxBulkStringBytes)
	}

	bufp := getBulkScratch(length)
	if _, err := io.ReadFull(r.reader, *bufp); err != nil {
		putBulkScratch(bufp)
		return v, err
	}
	if _, err := r.ReadLine(); err != nil {
		putBulkScratch(bufp)
		return v, err
	}
	v.Str = string(*bufp)
	putBulkScratch(bufp)
	return v, nil
}

func (r *Reader) readMap() (Value, error) {
	v := Value{Type: Map}
	count, _, err := r.readInteger()
	if err != nil {
		return v, err
	}
	if count < 0 {
		v.IsNull = true
		return v, nil
	}
	if count > maxArrayElements/2 {
		return v, fmt.Errorf("resp: map too large: %d (max %d)", count, maxArrayElements/2)
	}
	v.Array = make([]Value, count*2)
	for i := 0; i < count*2; i++ {
		val, err := r.Read()
		if err != nil {
			return v, err
		}
		v.Array[i] = val
	}
	return v, nil
}

func (r *Reader) readSet() (Value, error) {
	v := Value{Type: Set}
	count, _, err := r.readInteger()
	if err != nil {
		return v, err
	}
	if count < 0 {
		v.IsNull = true
		return v, nil
	}
	if count > maxArrayElements {
		return v, fmt.Errorf("resp: set too large: %d (max %d)", count, maxArrayElements)
	}
	v.Array = make([]Value, count)
	for i := 0; i < count; i++ {
		val, err := r.Read()
		if err != nil {
			return v, err
		}
		v.Array[i] = val
	}
	return v, nil
}

type Writer struct {
	writer *bufio.Writer
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{writer: bufio.NewWriterSize(w, defaultWriterBufSize)}
}

func (w *Writer) Flush() error {
	return w.writer.Flush()
}

func (w *Writer) Write(v Value) error {
	bufp := getScratch()
	// Reset + release after every exit path. The buffer is fully consumed by
	// the synchronous w.writer.Write below — holding the scratch slot as
	// len=0 (rather than handing it back populated) keeps the pool slot safe
	// against any future zero-copy path that might hold a reference to buf.
	defer func() {
		*bufp = (*bufp)[:0]
		putScratch(bufp)
	}()

	buf, err := appendValue((*bufp)[:0], v)
	*bufp = buf // retain any grown capacity so a subsequent Write can reuse it
	if err != nil {
		return err
	}

	if _, err := w.writer.Write(buf); err != nil {
		return fmt.Errorf("resp write: %w", err)
	}
	return nil
}

// appendValue appends the RESP-encoded form of v to b and returns the extended
// slice. Errors are returned only for unknown types; the append itself cannot
// fail. This is the single recursion point shared by all container types — the
// same scratch buffer flows through Array/Map/Set children without any
// intermediate allocation.
func appendValue(b []byte, v Value) ([]byte, error) {
	switch v.Type {
	case Array:
		return appendArray(b, v)
	case BulkString:
		return appendBulkString(b, v), nil
	case SimpleString:
		return appendLine(b, SimpleString, v.Str), nil
	case Error:
		return appendLine(b, Error, v.Str), nil
	case Integer:
		return appendInt(b, v), nil
	case Null:
		return append(b, "_\r\n"...), nil
	case Boolean:
		return appendBoolean(b, v), nil
	case Double:
		return appendDouble(b, v), nil
	case BulkError:
		return appendBulkError(b, v), nil
	case Map:
		return appendMap(b, v)
	case Set:
		return appendSet(b, v)
	default:
		return b, ErrUnknownType
	}
}

func appendArray(b []byte, v Value) ([]byte, error) {
	b = append(b, Array)
	b = strconv.AppendInt(b, int64(len(v.Array)), 10)
	b = append(b, '\r', '\n')
	for i := range v.Array {
		var err error
		b, err = appendValue(b, v.Array[i])
		if err != nil {
			return b, err
		}
	}
	return b, nil
}

func appendMap(b []byte, v Value) ([]byte, error) {
	pairs := len(v.Array) / 2
	b = append(b, Map)
	b = strconv.AppendInt(b, int64(pairs), 10)
	b = append(b, '\r', '\n')
	for i := range v.Array {
		var err error
		b, err = appendValue(b, v.Array[i])
		if err != nil {
			return b, err
		}
	}
	return b, nil
}

func appendSet(b []byte, v Value) ([]byte, error) {
	b = append(b, Set)
	b = strconv.AppendInt(b, int64(len(v.Array)), 10)
	b = append(b, '\r', '\n')
	for i := range v.Array {
		var err error
		b, err = appendValue(b, v.Array[i])
		if err != nil {
			return b, err
		}
	}
	return b, nil
}

func appendBulkString(b []byte, v Value) []byte {
	if v.IsNull {
		return append(b, "$-1\r\n"...)
	}
	b = append(b, BulkString)
	b = strconv.AppendInt(b, int64(len(v.Str)), 10)
	b = append(b, '\r', '\n')
	b = append(b, v.Str...)
	return append(b, '\r', '\n')
}

func appendLine(b []byte, prefix byte, s string) []byte {
	b = append(b, prefix)
	b = append(b, s...)
	return append(b, '\r', '\n')
}

func appendInt(b []byte, v Value) []byte {
	b = append(b, Integer)
	b = strconv.AppendInt(b, int64(v.Integer), 10)
	return append(b, '\r', '\n')
}

func appendBoolean(b []byte, v Value) []byte {
	if v.Bool {
		return append(b, "#t\r\n"...)
	}
	return append(b, "#f\r\n"...)
}

func appendDouble(b []byte, v Value) []byte {
	b = append(b, Double)
	b = strconv.AppendFloat(b, v.Float64, 'g', -1, 64)
	return append(b, '\r', '\n')
}

func appendBulkError(b []byte, v Value) []byte {
	b = append(b, BulkError)
	b = strconv.AppendInt(b, int64(len(v.Str)), 10)
	b = append(b, '\r', '\n')
	b = append(b, v.Str...)
	return append(b, '\r', '\n')
}

// Helper functions for easy value creation
func MarshalBulkString(s string) Value {
	return Value{Type: BulkString, Str: s}
}

func MarshalInt(i int) Value {
	return Value{Type: Integer, Integer: i}
}

func MarshalError(s string) Value {
	return Value{Type: Error, Str: s}
}

// MarshalNull returns the RESP2 null bulk string ($-1\r\n). Equivalent to
// helpers.Nil(); prefer that name inside the resp package.
func MarshalNull() Value {
	return Nil()
}

func MarshalSimpleString(s string) Value {
	return Value{Type: SimpleString, Str: s}
}
