package pluginsdk

import (
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	gcpcv1 "gocache/api/gcpc/v1"
)

func TestContextReconstructorSeedsInitialContextAndAppliesDeltas(t *testing.T) {
	reconstructor := NewContextReconstructor()

	operation := &gcpcv1.TelemetryOperation{
		OperationId: "op-1",
		InitialContext: []*gcpcv1.Tag{
			{Key: []byte("tenant"), Value: []byte("acme")},
			{Key: []byte("role"), Value: []byte("reader")},
		},
		TelemetryItems: []*gcpcv1.TelemetryItem{
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_UPDATE, encodeTelemetryPairs(t,
				"role", "writer",
				"trace", "abc",
			)),
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_REMOVE, encodeTelemetryKeys(t, "tenant")),
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH, encodeTelemetryPairs(t,
				telemetryFieldStatus, "completed",
			)),
		},
	}

	reconstructed := reconstructor.ProcessOperation(operation)
	if reconstructed == nil {
		t.Fatal("expected completed reconstruction")
	}

	wantContext := map[string]string{"role": "writer", "trace": "abc"}
	if !reflect.DeepEqual(reconstructed.Context, wantContext) {
		t.Fatalf("Context = %#v, want %#v", reconstructed.Context, wantContext)
	}
}

func TestContextReconstructorOperationFinishReturnsOutput(t *testing.T) {
	reconstructor := NewContextReconstructor()

	operation := &gcpcv1.TelemetryOperation{
		OperationId: "op-2",
		InitialContext: []*gcpcv1.Tag{
			{Key: []byte("request"), Value: []byte("req-1")},
		},
		TelemetryItems: []*gcpcv1.TelemetryItem{
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_START, encodeTelemetryPairs(t,
				telemetryFieldCommand, "SET",
				telemetryFieldArgsCount, "2",
				telemetryFieldArgPrefix+"0", "key",
				telemetryFieldArgPrefix+"1", "value",
			)),
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_UPDATE, encodeTelemetryPairs(t,
				"result.context", "updated",
			)),
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_LOG, encodeTelemetryPairs(t,
				telemetryFieldLevel, "info",
				telemetryFieldMessage, "command completed",
				telemetryFieldCaller, "pipeline.go:419",
			)),
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_EVENT, encodeTelemetryPairs(t,
				"_type", "command.completed",
				telemetryFieldCommand, "SET",
			)),
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_FINISH, encodeTelemetryPairs(t,
				telemetryFieldCommand, "SET",
				telemetryFieldElapsedNs, "123",
				telemetryFieldResult, "OK",
			)),
			telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH, encodeTelemetryPairs(t,
				telemetryFieldElapsedNs, "456",
				telemetryFieldStatus, "completed",
			)),
		},
	}

	reconstructed := reconstructor.ProcessOperation(operation)
	if reconstructed == nil {
		t.Fatal("expected completed reconstruction")
	}
	if reconstructed.OperationID != "op-2" {
		t.Fatalf("OperationID = %q, want op-2", reconstructed.OperationID)
	}
	if reconstructed.Elapsed != 456*time.Nanosecond {
		t.Fatalf("Elapsed = %s, want 456ns", reconstructed.Elapsed)
	}
	if reconstructed.Status != "completed" {
		t.Fatalf("Status = %q, want completed", reconstructed.Status)
	}

	wantContext := map[string]string{"request": "req-1", "result.context": "updated"}
	if !reflect.DeepEqual(reconstructed.Context, wantContext) {
		t.Fatalf("Context = %#v, want %#v", reconstructed.Context, wantContext)
	}

	wantCommands := []ReconstructedCommand{{
		Name:    "SET",
		Args:    []string{"key", "value"},
		Elapsed: 123 * time.Nanosecond,
		Result:  "OK",
	}}
	if !reflect.DeepEqual(reconstructed.Commands, wantCommands) {
		t.Fatalf("Commands = %#v, want %#v", reconstructed.Commands, wantCommands)
	}

	wantLogs := []ReconstructedLog{{Level: "info", Message: "command completed", Caller: "pipeline.go:419"}}
	if !reflect.DeepEqual(reconstructed.Logs, wantLogs) {
		t.Fatalf("Logs = %#v, want %#v", reconstructed.Logs, wantLogs)
	}

	wantEvents := []ReconstructedEvent{{Type: "command.completed", Data: map[string]string{telemetryFieldCommand: "SET"}}}
	if !reflect.DeepEqual(reconstructed.Events, wantEvents) {
		t.Fatalf("Events = %#v, want %#v", reconstructed.Events, wantEvents)
	}
}

func TestContextReconstructorTracksConcurrentOperationsByID(t *testing.T) {
	reconstructor := NewContextReconstructor()
	operationIDs := []string{"op-a", "op-b", "op-c", "op-d"}

	var waitGroup sync.WaitGroup
	for operationIndex, operationID := range operationIDs {
		waitGroup.Add(1)
		go func(operationIndex int, operationID string) {
			defer waitGroup.Done()
			partial := &gcpcv1.TelemetryOperation{
				OperationId: operationID,
				InitialContext: []*gcpcv1.Tag{
					{Key: []byte("operation"), Value: []byte(operationID)},
				},
				TelemetryItems: []*gcpcv1.TelemetryItem{
					telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_UPDATE, encodeTelemetryPairs(t,
						"index", strconv.Itoa(operationIndex),
					)),
				},
			}
			if reconstructed := reconstructor.ProcessOperation(partial); reconstructed != nil {
				t.Errorf("ProcessOperation partial for %s returned %#v, want nil", operationID, reconstructed)
			}
		}(operationIndex, operationID)
	}
	waitGroup.Wait()

	for operationIndex, operationID := range operationIDs {
		reconstructed := reconstructor.ProcessOperation(&gcpcv1.TelemetryOperation{
			OperationId: operationID,
			TelemetryItems: []*gcpcv1.TelemetryItem{
				telemetryItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH, encodeTelemetryPairs(t,
					telemetryFieldStatus, "completed",
				)),
			},
		})
		if reconstructed == nil {
			t.Fatalf("ProcessOperation finish for %s returned nil", operationID)
		}
		if reconstructed.Context["operation"] != operationID {
			t.Fatalf("operation context for %s = %q", operationID, reconstructed.Context["operation"])
		}
		if reconstructed.Context["index"] != strconv.Itoa(operationIndex) {
			t.Fatalf("index context for %s = %q", operationID, reconstructed.Context["index"])
		}
	}
}

func telemetryItem(kind gcpcv1.TelemetryItemKind, payload []byte) *gcpcv1.TelemetryItem {
	return &gcpcv1.TelemetryItem{Kind: kind, Payload: payload}
}

func encodeTelemetryPairs(t *testing.T, pairs ...string) []byte {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatalf("encodeTelemetryPairs requires even key/value entries, got %d", len(pairs))
	}
	payload := make([]byte, 1)
	for pairIndex := 0; pairIndex+1 < len(pairs); pairIndex += 2 {
		key := pairs[pairIndex]
		fieldText := pairs[pairIndex+1]
		if len(key) > 255 || len(fieldText) > 255 {
			t.Fatalf("pair %q=%q exceeds one-byte length", key, fieldText)
		}
		payload = append(payload, byte(len(key)))
		payload = append(payload, key...)
		payload = append(payload, byte(len(fieldText)))
		payload = append(payload, fieldText...)
		payload[0]++
	}
	return payload
}

func encodeTelemetryKeys(t *testing.T, keys ...string) []byte {
	t.Helper()
	payload := make([]byte, 1)
	for _, key := range keys {
		if len(key) > 255 {
			t.Fatalf("key %q exceeds one-byte length", key)
		}
		payload = append(payload, byte(len(key)))
		payload = append(payload, key...)
		payload[0]++
	}
	return payload
}
