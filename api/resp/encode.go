package resp

import "strconv"

const (
	SimpleString = '+'
	Error        = '-'
	Integer      = ':'
	BulkString   = '$'
	Array        = '*'
	Null         = '_'
	Boolean      = '#'
	Double       = ','
	BulkError    = '!'
	Map          = '%'
	Set          = '~'
)

// --- Append-based API (zero-alloc hot path) ---

func AppendBulkString(b []byte, s string) []byte {
	b = append(b, BulkString)
	b = strconv.AppendInt(b, int64(len(s)), 10)
	b = append(b, '\r', '\n')
	b = append(b, s...)
	return append(b, '\r', '\n')
}

func AppendInteger(b []byte, n int64) []byte {
	b = append(b, Integer)
	b = strconv.AppendInt(b, n, 10)
	return append(b, '\r', '\n')
}

func AppendSimpleString(b []byte, s string) []byte {
	b = append(b, SimpleString)
	b = append(b, s...)
	return append(b, '\r', '\n')
}

func AppendError(b []byte, s string) []byte {
	b = append(b, Error)
	b = append(b, s...)
	return append(b, '\r', '\n')
}

func AppendArrayHeader(b []byte, count int) []byte {
	b = append(b, Array)
	b = strconv.AppendInt(b, int64(count), 10)
	return append(b, '\r', '\n')
}

func AppendNullBulk(b []byte) []byte {
	return append(b, "$-1\r\n"...)
}

func AppendNullArray(b []byte) []byte {
	return append(b, "*-1\r\n"...)
}

// --- Allocating convenience API ---

func EncodeBulkString(s string) []byte {
	return AppendBulkString(make([]byte, 0, 1+20+2+len(s)+2), s)
}

func EncodeInteger(n int64) []byte {
	return AppendInteger(make([]byte, 0, 24), n)
}

func EncodeSimpleString(s string) []byte {
	return AppendSimpleString(make([]byte, 0, 1+len(s)+2), s)
}

func EncodeError(s string) []byte {
	return AppendError(make([]byte, 0, 1+len(s)+2), s)
}

func EncodeArray(elements ...[]byte) []byte {
	total := 0
	for _, e := range elements {
		total += len(e)
	}
	b := make([]byte, 0, 1+20+2+total)
	b = append(b, Array)
	b = strconv.AppendInt(b, int64(len(elements)), 10)
	b = append(b, '\r', '\n')
	for _, e := range elements {
		b = append(b, e...)
	}
	return b
}

func EncodeNullBulk() []byte {
	return []byte("$-1\r\n")
}

func EncodeNullArray() []byte {
	return []byte("*-1\r\n")
}
