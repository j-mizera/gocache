package resp

import "errors"

// Sentinel errors for type-safe error handling via errors.Is.
var (
	ErrWrongType  = errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	ErrNotInteger = errors.New("value is not an integer or out of range")
	ErrNotFloat   = errors.New("value is not a valid float")
)

func OK() Value     { return Value{Type: SimpleString, Str: "OK"} }
func Queued() Value { return Value{Type: SimpleString, Str: "QUEUED"} }
func Nil() Value    { return Value{Type: BulkString, IsNull: true} }

func ErrWrongTypeValue() Value {
	return MarshalError("WRONGTYPE Operation against a key holding the wrong kind of value")
}

func ErrUnknown(cmd string) Value {
	return MarshalError("ERR unknown command '" + cmd + "'")
}

func ErrArgs(cmd string) Value {
	return MarshalError("ERR wrong number of arguments for '" + cmd + "' command")
}

func ErrNotIntegerValue() Value {
	return MarshalError("ERR value is not an integer or out of range")
}

func ErrNotFloatValue() Value {
	return MarshalError("ERR value is not a valid float")
}

func ErrSyntax() Value {
	return MarshalError("ERR syntax error")
}

// StringArray wraps a []string as a RESP array of bulk strings.
func StringArray(values []string) Value {
	arr := make([]Value, len(values))
	for i, s := range values {
		arr[i] = MarshalBulkString(s)
	}
	return Value{Type: Array, Array: arr}
}

// ValueArray builds a RESP array from a variadic list of Values.
func ValueArray(values ...Value) Value {
	return Value{Type: Array, Array: values}
}

// IsNullValue reports whether the value represents a RESP null bulk string.
func (v Value) IsNullValue() bool {
	return v.IsNull
}

// RESP3 helper constructors

func NullV3() Value                 { return Value{Type: Null, IsNull: true} }
func True() Value                   { return Value{Type: Boolean, Bool: true} }
func False() Value                  { return Value{Type: Boolean, Bool: false} }
func MarshalDouble(f float64) Value { return Value{Type: Double, Float64: f} }

func MarshalBulkError(s string) Value {
	return Value{Type: BulkError, Str: s}
}

func MapFromPairs(pairs ...Value) Value {
	return Value{Type: Map, Array: pairs}
}

func SetFromValues(elements ...Value) Value {
	return Value{Type: Set, Array: elements}
}
