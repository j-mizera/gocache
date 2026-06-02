// Package observability provides reusable observability primitives that can be
// shared by the server, SDK, and plugin tooling without importing server
// internals.
package observability

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	apiobs "gocache/api/observability"

	"github.com/google/uuid"
)

var (
	// ErrZeroTraceID reports that a W3C trace id could not be made non-zero.
	ErrZeroTraceID = errors.New("observability: zero W3C trace id")
	// ErrZeroSpanID reports that a W3C span id could not be made non-zero.
	ErrZeroSpanID = errors.New("observability: zero W3C span id")
)

// UUIDv7Strategy renders public operation ids as UUIDv7 strings.
type UUIDv7Strategy struct{}

// Render returns a UUIDv7 operation identity. UUIDv7 is an export format only;
// the internal operation handle remains the ordering and sharding source.
func (UUIDv7Strategy) Render(input apiobs.OperationIdentityInput) (apiobs.OperationIdentity, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return apiobs.OperationIdentity{}, fmt.Errorf("render uuidv7 operation identity: %w", err)
	}
	return apiobs.OperationIdentity{
		ID:           apiobs.OperationID(id.String()),
		ParentID:     input.Parent.ID,
		TraceID:      input.TraceID,
		SpanID:       input.SpanID,
		ParentSpanID: input.ParentSpanID,
		TraceFlags:   input.TraceFlags,
	}, nil
}

// W3CTraceContextStrategy renders operation identity as a traceparent-compatible
// string: 00-<trace-id>-<span-id>-<flags>.
type W3CTraceContextStrategy struct {
	random io.Reader
}

// NewW3CTraceContextStrategy returns a W3C strategy backed by crypto/rand.
func NewW3CTraceContextStrategy() W3CTraceContextStrategy {
	return W3CTraceContextStrategy{random: rand.Reader}
}

// NewW3CTraceContextStrategyFromReader returns a W3C strategy backed by reader.
// It is intended for deterministic tests; nil falls back to crypto/rand.
func NewW3CTraceContextStrategyFromReader(reader io.Reader) W3CTraceContextStrategy {
	if reader == nil {
		reader = rand.Reader
	}
	return W3CTraceContextStrategy{random: reader}
}

// Render returns a W3C traceparent-compatible operation identity. The trace id
// and span id remain separate fields so tracing semantics are not flattened into
// a trace-id-only operation tree.
func (s W3CTraceContextStrategy) Render(input apiobs.OperationIdentityInput) (apiobs.OperationIdentity, error) {
	reader := s.random
	if reader == nil {
		reader = rand.Reader
	}

	traceID := input.TraceID
	if isZero(traceID[:]) {
		if err := fillNonZero(reader, traceID[:], ErrZeroTraceID); err != nil {
			return apiobs.OperationIdentity{}, err
		}
	}

	spanID := input.SpanID
	if isZero(spanID[:]) {
		if err := fillNonZero(reader, spanID[:], ErrZeroSpanID); err != nil {
			return apiobs.OperationIdentity{}, err
		}
	}

	var traceparent [55]byte
	traceparent[0] = '0'
	traceparent[1] = '0'
	traceparent[2] = '-'
	hex.Encode(traceparent[3:35], traceID[:])
	traceparent[35] = '-'
	hex.Encode(traceparent[36:52], spanID[:])
	traceparent[52] = '-'
	writeLowerHexByte(traceparent[53:55], byte(input.TraceFlags))

	return apiobs.OperationIdentity{
		ID:           apiobs.OperationID(string(traceparent[:])),
		ParentID:     input.Parent.ID,
		TraceID:      traceID,
		SpanID:       spanID,
		ParentSpanID: input.ParentSpanID,
		TraceFlags:   input.TraceFlags,
	}, nil
}

func fillNonZero(reader io.Reader, dst []byte, zeroErr error) error {
	if _, err := io.ReadFull(reader, dst); err != nil {
		return fmt.Errorf("render W3C operation identity: %w", err)
	}
	if isZero(dst) {
		return zeroErr
	}
	return nil
}

func isZero(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}

func writeLowerHexByte(dst []byte, b byte) {
	const digits = "0123456789abcdef"
	dst[0] = digits[b>>4]
	dst[1] = digits[b&0x0f]
}
