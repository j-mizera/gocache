package observability

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	apiobs "gocache/api/observability"

	"github.com/google/uuid"
)

func TestUUIDv7StrategyRender(t *testing.T) {
	identity, err := (UUIDv7Strategy{}).Render(apiobs.OperationIdentityInput{
		Parent: apiobs.NewOperationRef("parent", ""),
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	parsed, err := uuid.Parse(identity.ID.String())
	if err != nil {
		t.Fatalf("parse UUIDv7 identity: %v", err)
	}
	if parsed.Version() != 7 {
		t.Fatalf("Version() = %d, want 7", parsed.Version())
	}
	if parsed.Variant() != uuid.RFC4122 {
		t.Fatalf("Variant() = %v, want RFC4122", parsed.Variant())
	}
	if identity.ParentID != "parent" {
		t.Fatalf("ParentID = %q, want parent", identity.ParentID)
	}
	if parsed.Time() == 0 {
		t.Fatal("UUIDv7 Time() should be non-zero")
	}
}

func TestW3CTraceContextStrategyUsesProvidedTraceAndSpan(t *testing.T) {
	traceID := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := [8]byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	parentSpanID := [8]byte{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28}

	identity, err := NewW3CTraceContextStrategyFromReader(bytes.NewReader(nil)).Render(apiobs.OperationIdentityInput{
		Parent:       apiobs.NewOperationRef("parent", ""),
		TraceID:      traceID,
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
		TraceFlags:   0x01,
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	want := "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01"
	if identity.ID.String() != want {
		t.Fatalf("identity = %q, want %q", identity.ID, want)
	}
	if identity.TraceID != traceID || identity.SpanID != spanID || identity.ParentSpanID != parentSpanID {
		t.Fatalf("trace fields were not preserved: %+v", identity)
	}
	if identity.ParentID != "parent" {
		t.Fatalf("ParentID = %q, want parent", identity.ParentID)
	}
}

func TestW3CTraceContextStrategyGeneratesMissingTraceAndSpan(t *testing.T) {
	generated := append([]byte{}, strings.Repeat("a", 16)...)
	generated = append(generated, []byte("spanseed")...)

	identity, err := NewW3CTraceContextStrategyFromReader(bytes.NewReader(generated)).Render(apiobs.OperationIdentityInput{})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	want := "00-61616161616161616161616161616161-7370616e73656564-00"
	if identity.ID.String() != want {
		t.Fatalf("identity = %q, want %q", identity.ID, want)
	}
	if isZero(identity.TraceID[:]) {
		t.Fatal("TraceID should be generated")
	}
	if isZero(identity.SpanID[:]) {
		t.Fatal("SpanID should be generated")
	}
}

func TestW3CTraceContextStrategyRejectsGeneratedZeroTraceID(t *testing.T) {
	_, err := NewW3CTraceContextStrategyFromReader(bytes.NewReader(make([]byte, 16))).Render(apiobs.OperationIdentityInput{})
	if !errors.Is(err, ErrZeroTraceID) {
		t.Fatalf("Render() error = %v, want ErrZeroTraceID", err)
	}
}
