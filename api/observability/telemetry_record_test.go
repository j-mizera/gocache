package observability

import (
	"reflect"
	"testing"
)

func TestTelemetryRecordPointerFree(t *testing.T) {
	if field := firstPointerLikeField(reflect.TypeOf(TelemetryRecord{}), "TelemetryRecord"); field != "" {
		t.Fatalf("TelemetryRecord must remain pointer-free, found %s", field)
	}
}

func TestParentRefPointerFree(t *testing.T) {
	if field := firstPointerLikeField(reflect.TypeOf(ParentRef{}), "ParentRef"); field != "" {
		t.Fatalf("ParentRef must remain pointer-free, found %s", field)
	}
}

func firstPointerLikeField(t reflect.Type, path string) string {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface, reflect.Chan, reflect.Func, reflect.String:
		return path + " (" + t.Kind().String() + ")"
	case reflect.Array:
		return firstPointerLikeField(t.Elem(), path+"[]")
	case reflect.Struct:
		for i := range t.NumField() {
			field := t.Field(i)
			if nested := firstPointerLikeField(field.Type, path+"."+field.Name); nested != "" {
				return nested
			}
		}
	}
	return ""
}

func TestTelemetryRecordSetNameAndPayload(t *testing.T) {
	record := NewTelemetryRecord(TelemetryRecordLog, 7)
	if retained := record.SetName([]byte("debug message")); retained != len("debug message") {
		t.Fatalf("SetName retained %d bytes, want %d", retained, len("debug message"))
	}
	if retained := record.SetPayload([]byte("hello")); retained != 5 {
		t.Fatalf("SetPayload retained %d bytes, want 5", retained)
	}
	if got := string(record.NameBytes()); got != "debug message" {
		t.Fatalf("NameBytes() = %q, want debug message", got)
	}
	if got := string(record.PayloadBytes()); got != "hello" {
		t.Fatalf("PayloadBytes() = %q, want hello", got)
	}
}

func TestTelemetryRecordSetNameAndPayloadTruncate(t *testing.T) {
	record := NewTelemetryRecord(TelemetryRecordEvent, 9)
	name := make([]byte, TelemetryNameBytes+3)
	payload := make([]byte, TelemetryPayloadBytes+3)
	for i := range name {
		name[i] = 'n'
	}
	for i := range payload {
		payload[i] = 'x'
	}
	if retained := record.SetName(name); retained != TelemetryNameBytes {
		t.Fatalf("SetName retained %d bytes, want %d", retained, TelemetryNameBytes)
	}
	if retained := record.SetPayload(payload); retained != TelemetryPayloadBytes {
		t.Fatalf("SetPayload retained %d bytes, want %d", retained, TelemetryPayloadBytes)
	}
	if len(record.NameBytes()) != TelemetryNameBytes {
		t.Fatalf("NameBytes len = %d, want %d", len(record.NameBytes()), TelemetryNameBytes)
	}
	if len(record.PayloadBytes()) != TelemetryPayloadBytes {
		t.Fatalf("PayloadBytes len = %d, want %d", len(record.PayloadBytes()), TelemetryPayloadBytes)
	}
}

func TestParentRefCopiesInput(t *testing.T) {
	parent := []byte("parent-operation")
	ref := NewParentRefBytes(parent)
	copy(parent, "mutated-parent")
	if got := ref.String(); got != "parent-operation" {
		t.Fatalf("ParentRef = %q, want parent-operation", got)
	}
}

func TestNewLogRecordBytesCopiesMessageAndKeepsFlags(t *testing.T) {
	message := []byte("startup ready")
	record := NewLogRecordBytes(42, TelemetryLogLevelInfo, message)
	record.Flags |= TelemetryRecordFlagLocalLogMaterialized
	copy(message, "mutated")

	if record.Kind != TelemetryRecordLog {
		t.Fatalf("kind = %v, want log", record.Kind)
	}
	if record.Operation != 42 {
		t.Fatalf("operation = %d, want 42", record.Operation)
	}
	if record.Level != TelemetryLogLevelInfo {
		t.Fatalf("level = %v, want info", record.Level)
	}
	if got := string(record.NameBytes()); got != "startup ready" {
		t.Fatalf("message = %q, want startup ready", got)
	}
	if record.Flags&TelemetryRecordFlagLocalLogMaterialized == 0 {
		t.Fatal("local materialized flag should be retained")
	}
}

func TestTelemetryRecordAddFieldBytesCopiesBoundedKV(t *testing.T) {
	record := NewLogRecordBytes(7, TelemetryLogLevelWarn, []byte("eviction skipped"))
	key := []byte("reason")
	value := []byte("memory-limit")
	if !record.AddFieldBytes(key, value) {
		t.Fatal("AddFieldBytes should fit")
	}
	copy(value, "mutated-value")

	if record.FieldCount != 1 {
		t.Fatalf("field count = %d, want 1", record.FieldCount)
	}
	gotKey, gotValue, ok := record.FieldBytes(0)
	if !ok {
		t.Fatal("FieldBytes(0) should return field")
	}
	if string(gotKey) != "reason" || string(gotValue) != "memory-limit" {
		t.Fatalf("field = %q:%q, want reason:memory-limit", gotKey, gotValue)
	}
	if _, _, ok := record.FieldBytes(1); ok {
		t.Fatal("FieldBytes(1) should report no field")
	}
}

func TestTelemetryRecordAddFieldBytesDropsWhenPayloadFull(t *testing.T) {
	record := NewLogRecordBytes(7, TelemetryLogLevelWarn, []byte("overflow"))
	large := make([]byte, TelemetryPayloadBytes)
	for i := range large {
		large[i] = 'x'
	}
	if record.AddFieldBytes([]byte("too-large"), large) {
		t.Fatal("AddFieldBytes should fail when field exceeds payload")
	}
	if record.FieldCount != 0 {
		t.Fatalf("field count = %d, want 0", record.FieldCount)
	}
	if record.DroppedFields != 1 {
		t.Fatalf("dropped fields = %d, want 1", record.DroppedFields)
	}
}

func TestTelemetryRecordAddFieldStringAvoidsTemporaryByteSlice(t *testing.T) {
	record := NewLogRecordString(9, TelemetryLogLevelDebug, "debug message")
	if !record.AddFieldString("plugin", "aof") {
		t.Fatal("AddFieldString should fit")
	}
	key, value, ok := record.FieldBytes(0)
	if !ok {
		t.Fatal("FieldBytes(0) should return field")
	}
	if string(record.NameBytes()) != "debug message" || string(key) != "plugin" || string(value) != "aof" {
		t.Fatalf("record = %q %q=%q, want debug message plugin=aof", record.NameBytes(), key, value)
	}
}
