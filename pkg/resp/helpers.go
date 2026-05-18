package resp

import (
	"errors"

	apicommand "gocache/api/command"
)

// Sentinel errors aliased from api/command — the canonical definitions
// live there so plugins can reference them without importing pkg/.
// Aliases preserve errors.Is identity (same underlying pointer).
var (
	ErrWrongType         = apicommand.ErrWrongType
	ErrNotInteger        = apicommand.ErrNotInteger
	ErrNotFloat          = apicommand.ErrNotFloat
	ErrInvalidExpireTime = apicommand.ErrInvalidExpireTime
	ErrInvalidTimeout    = apicommand.ErrInvalidTimeout

	ErrUnknownType = errors.New("unknown RESP value type")
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

func ErrSyntax() Value    { return MarshalError("ERR syntax error") }
func ErrNoSuchKey() Value { return MarshalError("ERR no such key") }

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
