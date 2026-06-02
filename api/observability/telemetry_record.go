package observability

// TelemetryNameBytes is the fixed inline capacity for dynamic command names,
// log messages, and event names. Names may be registered by plugins at runtime,
// so the hot path carries bytes instead of assuming preassigned ids.
const TelemetryNameBytes = 64

// TelemetryPayloadBytes is the fixed inline payload capacity for allocation-free
// telemetry records. Larger values should be encoded by a worker-owned side
// store, not by adding pointers to TelemetryRecord.
const TelemetryPayloadBytes = 128

// TelemetryParentIDBytes is the fixed inline capacity for exported parent
// operation ids on operation-start records. It fits UUIDv7 strings and W3C
// traceparent-compatible ids without carrying a string or []byte pointer.
const TelemetryParentIDBytes = 64

// ParentRef carries an exported parent operation id inline. It is used only for
// operation causality at operation start; logs and events are already under an
// operation and do not carry a parent reference on the submit path.
type ParentRef struct {
	ParentLen uint16
	ParentID  [TelemetryParentIDBytes]byte
}

// NewParentRef returns an inline parent reference copied from an exported id.
func NewParentRef(parentID string) ParentRef {
	var ref ParentRef
	ref.SetString(parentID)
	return ref
}

// NewParentRefBytes returns an inline parent reference copied from bytes.
func NewParentRefBytes(parentID []byte) ParentRef {
	var ref ParentRef
	ref.SetBytes(parentID)
	return ref
}

// SetString copies parentID into the inline buffer and returns bytes retained.
// It does not retain the input string.
func (r *ParentRef) SetString(parentID string) int {
	n := copy(r.ParentID[:], parentID)
	r.ParentLen = uint16(n)
	return n
}

// SetBytes copies parentID into the inline buffer and returns bytes retained.
// It does not retain the input slice.
func (r *ParentRef) SetBytes(parentID []byte) int {
	n := copy(r.ParentID[:], parentID)
	r.ParentLen = uint16(n)
	return n
}

// IsZero reports whether no parent id is present.
func (r ParentRef) IsZero() bool { return r.ParentLen == 0 }

// Len returns the retained parent id length.
func (r ParentRef) Len() int { return int(r.ParentLen) }

// CopyTo copies the retained parent id into dst and returns bytes copied.
func (r ParentRef) CopyTo(dst []byte) int {
	return copy(dst, r.ParentID[:r.ParentLen])
}

// String returns the retained parent id. It is intended for diagnostics,
// boundaries, and tests, not the command hot path.
func (r ParentRef) String() string {
	return string(r.ParentID[:r.ParentLen])
}

// TelemetryRecordKind identifies the kind of compact telemetry request carried
// by a TelemetryRecord. Kinds describe worker-side work to do later; they are not
// materialized logs, events, GCPC messages, or plugin fanout objects.
type TelemetryRecordKind uint8

const (
	TelemetryRecordOperationStart TelemetryRecordKind = iota
	TelemetryRecordOperationFinish
	TelemetryRecordCommandStart
	TelemetryRecordCommandFinish
	TelemetryRecordLog
	TelemetryRecordContextUpdate
	TelemetryRecordContextRemove
	TelemetryRecordEvent
	TelemetryRecordDrop
)

func (k TelemetryRecordKind) String() string {
	switch k {
	case TelemetryRecordOperationStart:
		return "operation.start"
	case TelemetryRecordOperationFinish:
		return "operation.finish"
	case TelemetryRecordCommandStart:
		return "command.start"
	case TelemetryRecordCommandFinish:
		return "command.finish"
	case TelemetryRecordLog:
		return "log.request"
	case TelemetryRecordContextUpdate:
		return "context.update"
	case TelemetryRecordContextRemove:
		return "context.remove"
	case TelemetryRecordEvent:
		return "event.request"
	case TelemetryRecordDrop:
		return "drop"
	default:
		return "unknown"
	}
}

// TelemetryRecord is a pointer-free, fixed-size outbox request submitted into
// common sidecar primitives. It records telemetry intent/state in memory for
// later processing; it is not itself the final log/event/GCPC payload and does
// not perform emission or fanout. It carries only scalar ids, inline dynamic names, an
// inline parent reference for operation start, and an inline byte payload so
// ring storage does not retain request-owned buffers or force GC pointer
// scanning.
type TelemetryRecord struct {
	Kind              TelemetryRecordKind
	Level             TelemetryLogLevel
	NameLen           uint16
	PayloadLen        uint16
	Operation         InternalOperationIdentity
	TimestampUnixNano int64
	Number            int64
	ContextVersion    ConnectionContextVersion
	Parent            ParentRef
	Name              [TelemetryNameBytes]byte
	Payload           [TelemetryPayloadBytes]byte
}

// NewTelemetryRecord creates a compact telemetry record for operation.
func NewTelemetryRecord(kind TelemetryRecordKind, operation InternalOperationIdentity) TelemetryRecord {
	return TelemetryRecord{Kind: kind, Operation: operation}
}

// SetName copies data into the fixed inline name buffer and returns the number
// of bytes retained. Data longer than TelemetryNameBytes is truncated.
func (r *TelemetryRecord) SetName(data []byte) int {
	n := copy(r.Name[:], data)
	r.NameLen = uint16(n)
	return n
}

// NameBytes returns the retained inline dynamic name bytes.
func (r *TelemetryRecord) NameBytes() []byte {
	return r.Name[:r.NameLen]
}

// SetPayload copies data into the fixed inline payload buffer and returns the
// number of bytes retained. Data longer than TelemetryPayloadBytes is truncated.
func (r *TelemetryRecord) SetPayload(data []byte) int {
	n := copy(r.Payload[:], data)
	r.PayloadLen = uint16(n)
	return n
}

// PayloadBytes returns the retained inline payload bytes.
func (r *TelemetryRecord) PayloadBytes() []byte {
	return r.Payload[:r.PayloadLen]
}
