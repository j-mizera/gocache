package observability

import (
	"time"

	apiobs "gocache/api/observability"
)

// NewSingleProducerOperationTracker returns an injectable OperationTracker whose
// concrete implementation stays hidden inside commons/observability.
func NewSingleProducerOperationTracker(capacity int) apiobs.OperationTracker {
	return newOperationTracker(newSingleProducerRecorder(capacity))
}

type operationTracker struct {
	recorder telemetryRecorder
}

func newOperationTracker(recorder telemetryRecorder) *operationTracker {
	return &operationTracker{recorder: recorder}
}

func (t *operationTracker) RecordTelemetry(record apiobs.TelemetryRecord) bool {
	return t.recorder.RecordTelemetry(record)
}

func (t *operationTracker) DroppedRecords() uint64 {
	return t.recorder.DroppedRecords()
}

func (t *operationTracker) StartOperation(operation apiobs.InternalOperationIdentity, parent apiobs.ParentRef, contextVersion apiobs.ConnectionContextVersion) bool {
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordOperationStart, operation)
	record.Parent = parent
	record.ContextVersion = contextVersion
	record.TimestampUnixNano = nowUnixNano()
	return t.RecordTelemetry(record)
}

func (t *operationTracker) FinishOperation(operation apiobs.InternalOperationIdentity) bool {
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordOperationFinish, operation)
	record.TimestampUnixNano = nowUnixNano()
	return t.RecordTelemetry(record)
}

func (t *operationTracker) StartCommand(operation apiobs.InternalOperationIdentity, commandName []byte, payload []byte) bool {
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandStart, operation)
	record.TimestampUnixNano = nowUnixNano()
	record.SetName(commandName)
	record.SetPayload(payload)
	return t.RecordTelemetry(record)
}

func (t *operationTracker) FinishCommand(operation apiobs.InternalOperationIdentity, resultCode int64) bool {
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordCommandFinish, operation)
	record.Number = resultCode
	record.TimestampUnixNano = nowUnixNano()
	return t.RecordTelemetry(record)
}

func (t *operationTracker) Log(operation apiobs.InternalOperationIdentity, level apiobs.TelemetryLogLevel, message []byte, fields ...[]byte) bool {
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordLog, operation)
	record.Level = level
	record.TimestampUnixNano = nowUnixNano()
	record.SetName(message)
	record.PayloadLen = uint16(packKeyValues(record.Payload[:], fields))
	return t.RecordTelemetry(record)
}

func (t *operationTracker) Event(operation apiobs.InternalOperationIdentity, eventName []byte, fields ...[]byte) bool {
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordEvent, operation)
	record.TimestampUnixNano = nowUnixNano()
	record.SetName(eventName)
	record.PayloadLen = uint16(packKeyValues(record.Payload[:], fields))
	return t.RecordTelemetry(record)
}

func (t *operationTracker) ContextUpdate(operation apiobs.InternalOperationIdentity, pairs ...[]byte) bool {
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordContextUpdate, operation)
	record.TimestampUnixNano = nowUnixNano()
	record.PayloadLen = uint16(packKeyValues(record.Payload[:], pairs))
	return t.RecordTelemetry(record)
}

func (t *operationTracker) ContextRemove(operation apiobs.InternalOperationIdentity, keys ...[]byte) bool {
	record := apiobs.NewTelemetryRecord(apiobs.TelemetryRecordContextRemove, operation)
	record.TimestampUnixNano = nowUnixNano()
	record.PayloadLen = uint16(packKeys(record.Payload[:], keys))
	return t.RecordTelemetry(record)
}

func nowUnixNano() int64 { return time.Now().UnixNano() }

func packKeyValues(dst []byte, pairs [][]byte) int {
	if len(dst) == 0 {
		return 0
	}
	pos := 1
	count := 0
	for i := 0; i+1 < len(pairs); i += 2 {
		key := pairs[i]
		value := pairs[i+1]
		if len(key) > 255 || len(value) > 255 || pos+2+len(key)+len(value) > len(dst) {
			break
		}
		dst[pos] = byte(len(key))
		pos++
		pos += copy(dst[pos:], key)
		dst[pos] = byte(len(value))
		pos++
		pos += copy(dst[pos:], value)
		count++
	}
	dst[0] = byte(count)
	return pos
}

func packKeys(dst []byte, keys [][]byte) int {
	if len(dst) == 0 {
		return 0
	}
	pos := 1
	count := 0
	for _, key := range keys {
		if len(key) > 255 || pos+1+len(key) > len(dst) {
			break
		}
		dst[pos] = byte(len(key))
		pos++
		pos += copy(dst[pos:], key)
		count++
	}
	dst[0] = byte(count)
	return pos
}
