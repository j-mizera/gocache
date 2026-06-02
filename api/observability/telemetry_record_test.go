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
