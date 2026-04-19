package resp

import (
	"bufio"
	"errors"
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

	bulk := make([]byte, n)
	_, err = io.ReadFull(r.reader, bulk)
	if err != nil {
		return v, err
	}

	// Read CRLF
	if _, err := r.ReadLine(); err != nil {
		return v, err
	}

	v.Str = string(bulk)
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
	bulk := make([]byte, length)
	_, err = io.ReadFull(r.reader, bulk)
	if err != nil {
		return v, err
	}
	if _, err := r.ReadLine(); err != nil {
		return v, err
	}
	v.Str = string(bulk)
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
	var b []byte
	switch v.Type {
	case Array:
		b = w.marshalArray(v)
	case BulkString:
		b = w.marshalBulkString(v)
	case SimpleString:
		b = w.marshalSimpleString(v)
	case Error:
		b = w.marshalError(v)
	case Integer:
		b = w.marshalInt(v)
	case Null:
		b = w.marshalNull()
	case Boolean:
		b = w.marshalBoolean(v)
	case Double:
		b = w.marshalDouble(v)
	case BulkError:
		b = w.marshalBulkError(v)
	case Map:
		b = w.marshalMap(v)
	case Set:
		b = w.marshalSet(v)
	default:
		return errors.New("unknown type")
	}

	_, err := w.writer.Write(b)
	if err != nil {
		return fmt.Errorf("resp write: %w", err)
	}
	return nil
}

func (w *Writer) marshalArray(v Value) []byte {
	n := len(v.Array)
	var b []byte
	b = append(b, Array)
	b = append(b, strconv.Itoa(n)...)
	b = append(b, '\r', '\n')

	for i := 0; i < n; i++ {
		b = append(b, w.marshalValue(v.Array[i])...)
	}
	return b
}

func (w *Writer) marshalBulkString(v Value) []byte {
	if v.IsNull {
		return []byte("$-1\r\n")
	}
	var b []byte
	b = append(b, BulkString)
	b = append(b, strconv.Itoa(len(v.Str))...)
	b = append(b, '\r', '\n')
	b = append(b, v.Str...)
	b = append(b, '\r', '\n')
	return b
}

func (w *Writer) marshalSimpleString(v Value) []byte {
	var b []byte
	b = append(b, SimpleString)
	b = append(b, v.Str...)
	b = append(b, '\r', '\n')
	return b
}

func (w *Writer) marshalError(v Value) []byte {
	var b []byte
	b = append(b, Error)
	b = append(b, v.Str...)
	b = append(b, '\r', '\n')
	return b
}

func (w *Writer) marshalInt(v Value) []byte {
	var b []byte
	b = append(b, Integer)
	b = append(b, strconv.Itoa(v.Integer)...)
	b = append(b, '\r', '\n')
	return b
}

func (w *Writer) marshalNull() []byte {
	return []byte("_\r\n")
}

func (w *Writer) marshalBoolean(v Value) []byte {
	if v.Bool {
		return []byte("#t\r\n")
	}
	return []byte("#f\r\n")
}

func (w *Writer) marshalDouble(v Value) []byte {
	var b []byte
	b = append(b, Double)
	b = append(b, strconv.FormatFloat(v.Float64, 'g', -1, 64)...)
	b = append(b, '\r', '\n')
	return b
}

func (w *Writer) marshalBulkError(v Value) []byte {
	var b []byte
	b = append(b, BulkError)
	b = append(b, strconv.Itoa(len(v.Str))...)
	b = append(b, '\r', '\n')
	b = append(b, v.Str...)
	b = append(b, '\r', '\n')
	return b
}

func (w *Writer) marshalMap(v Value) []byte {
	pairs := len(v.Array) / 2
	var b []byte
	b = append(b, Map)
	b = append(b, strconv.Itoa(pairs)...)
	b = append(b, '\r', '\n')
	for i := 0; i < len(v.Array); i++ {
		b = append(b, w.marshalValue(v.Array[i])...)
	}
	return b
}

func (w *Writer) marshalSet(v Value) []byte {
	var b []byte
	b = append(b, Set)
	b = append(b, strconv.Itoa(len(v.Array))...)
	b = append(b, '\r', '\n')
	for i := 0; i < len(v.Array); i++ {
		b = append(b, w.marshalValue(v.Array[i])...)
	}
	return b
}

func (w *Writer) marshalValue(v Value) []byte {
	switch v.Type {
	case Array:
		return w.marshalArray(v)
	case BulkString:
		return w.marshalBulkString(v)
	case SimpleString:
		return w.marshalSimpleString(v)
	case Error:
		return w.marshalError(v)
	case Integer:
		return w.marshalInt(v)
	case Null:
		return w.marshalNull()
	case Boolean:
		return w.marshalBoolean(v)
	case Double:
		return w.marshalDouble(v)
	case BulkError:
		return w.marshalBulkError(v)
	case Map:
		return w.marshalMap(v)
	case Set:
		return w.marshalSet(v)
	default:
		return nil
	}
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

func MarshalNull() Value {
	return Value{Type: BulkString, IsNull: true}
}

func MarshalSimpleString(s string) Value {
	return Value{Type: SimpleString, Str: s}
}
