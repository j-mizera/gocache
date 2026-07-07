//go:build linux

package server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	gcpcv1 "gocache/api/gcpc/v1"
	apiobs "gocache/api/observability"
	commonobs "gocache/commons/observability"
	"gocache/sdk/pluginsdk"
)

const (
	telemetryTmpfsLengthPrefixSize = 4
	telemetryTestFieldArgsCount    = "_args_count"
	telemetryTestFieldArgPrefix    = "_arg."
	telemetryTestFieldCommand      = "_command"
	telemetryTestFieldElapsedNs    = "_elapsed_ns"
	telemetryTestFieldResult       = "_result"
	telemetryTestFieldStatus       = "_status"
)

func TestTelemetryTmpfsInitialContextIncludesFilteredOperationOverlay(t *testing.T) {
	if _, err := os.Stat(commonobs.TmpfsTelemetryDir); err != nil {
		t.Skipf("tmpfs telemetry directory unavailable: %v", err)
	}
	writer := newTelemetryTmpfsIntegrationWriter(t)
	manager := newTestSlotOperationTrackerManager(t, 1, 1)
	connection := apiobs.ConnectionIdentity(31)
	baseVersion := manager.UpdateConnectionContext(
		connection,
		[]byte("shared.rex.data"), []byte("base-value"),
		[]byte("tenant"), []byte("acme"),
	)
	overlay := map[string]string{
		"shared.rex.data":       "custom-value",
		"myplugin.private_data": "hidden",
	}
	handle, pinned, ok := manager.StartOperationWithConnectionContext(1, apiobs.NewParentRef("connection-op"), connection, overlay)
	if !ok {
		t.Fatal("StartOperationWithConnectionContext should allocate a slot")
	}
	if pinned != baseVersion {
		t.Fatalf("pinned context version = %d, want %d", pinned, baseVersion)
	}
	if !manager.FinishOperation(handle, commonobs.SlotTerminalFinished) {
		t.Fatal("FinishOperation should enqueue completed operation")
	}

	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetTmpfsWriter(io.MultiWriter(writer))
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want 1", drained)
	}

	fileBytes, err := os.ReadFile(writer.FilePath())
	if err != nil {
		t.Fatalf("ReadFile(tmpfs telemetry) error = %v", err)
	}
	writeOffset := writer.WriteOffset()
	if writeOffset <= telemetryTmpfsLengthPrefixSize {
		t.Fatalf("WriteOffset() = %d, want serialized operation entry", writeOffset)
	}
	payloadLength := binary.BigEndian.Uint32(fileBytes[commonobs.TmpfsTelemetryHeaderSize : commonobs.TmpfsTelemetryHeaderSize+telemetryTmpfsLengthPrefixSize])
	payloadStart := commonobs.TmpfsTelemetryHeaderSize + telemetryTmpfsLengthPrefixSize
	payloadEnd := payloadStart + int(payloadLength)
	if payloadEnd > commonobs.TmpfsTelemetryHeaderSize+int(writeOffset) {
		t.Fatalf("payload end = %d beyond write offset %d", payloadEnd, commonobs.TmpfsTelemetryHeaderSize+int(writeOffset))
	}
	var telemetryOperation gcpcv1.TelemetryOperation
	if err := telemetryOperation.UnmarshalVT(fileBytes[payloadStart:payloadEnd]); err != nil {
		t.Fatalf("Unmarshal(TelemetryOperation) error = %v", err)
	}

	sharedValues := []string{}
	for _, tag := range telemetryOperation.InitialContext {
		switch string(tag.Key) {
		case "shared.rex.data":
			sharedValues = append(sharedValues, string(tag.Value))
		case "myplugin.private_data":
			t.Fatalf("private overlay key %q should be filtered", string(tag.Key))
		case "tenant":
			t.Fatalf("non-prefixed base context key %q should be filtered by deny-by-default filter", string(tag.Key))
		}
	}
	if len(sharedValues) == 0 {
		t.Fatalf("initial_context missing shared.rex.data: %+v", telemetryOperation.InitialContext)
	}
	if lastValue := sharedValues[len(sharedValues)-1]; lastValue != "custom-value" {
		t.Fatalf("last shared.rex.data value = %q, want custom-value", lastValue)
	}
}

func TestTelemetryTmpfsEndToEndPipelineAndAck(t *testing.T) {
	writer := newTelemetryTmpfsIntegrationWriter(t)
	operation := &gcpcv1.TelemetryOperation{
		OperationId: "test-op-1",
		InitialContext: []*gcpcv1.Tag{
			{Key: []byte("tenant"), Value: []byte("acme")},
			{Key: []byte("role"), Value: []byte("reader")},
		},
		TelemetryItems: []*gcpcv1.TelemetryItem{
			newTelemetryTmpfsTestItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_START, nil),
			newTelemetryTmpfsTestItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_UPDATE, encodeTelemetryTmpfsPairs(t,
				"traceparent", "00-abc",
			)),
			newTelemetryTmpfsTestItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_START, encodeTelemetryTmpfsPairs(t,
				telemetryTestFieldCommand, "GET",
				telemetryTestFieldArgsCount, "1",
				telemetryTestFieldArgPrefix+"0", "user:1",
			)),
			newTelemetryTmpfsTestItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_COMMAND_FINISH, encodeTelemetryTmpfsPairs(t,
				telemetryTestFieldCommand, "GET",
				telemetryTestFieldElapsedNs, "1234",
				telemetryTestFieldResult, "hit",
			)),
			newTelemetryTmpfsTestItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH, encodeTelemetryTmpfsPairs(t,
				telemetryTestFieldStatus, "completed",
			)),
		},
	}
	expectedConsumedOffset := writeTelemetryTmpfsOperation(t, writer, operation)
	ackOffsets := make(chan uint64, 2)
	confirmations := make(chan bool, 2)

	telemetryPlugin := pluginsdk.NewTelemetryPlugin(
		writer.FilePath(),
		func(consumedOffset uint64) {
			writer.Acknowledge(consumedOffset)
			ackOffsets <- consumedOffset
		},
		func() error {
			compacted, err := writer.CompactIfNeeded()
			if err != nil {
				return err
			}
			confirmations <- compacted
			return nil
		},
	).WithPollInterval(time.Millisecond).WithThresholds(1, time.Hour)
	startTelemetryTmpfsPlugin(t, telemetryPlugin)

	reconstructedOperation := waitForTelemetryTmpfsOperation(t, telemetryPlugin.Operations())
	if reconstructedOperation.OperationID != "test-op-1" {
		t.Fatalf("OperationID = %q, want test-op-1", reconstructedOperation.OperationID)
	}
	if reconstructedOperation.Context["tenant"] != "acme" {
		t.Fatalf("tenant context = %q, want acme", reconstructedOperation.Context["tenant"])
	}
	if reconstructedOperation.Context["role"] != "reader" {
		t.Fatalf("role context = %q, want reader", reconstructedOperation.Context["role"])
	}
	if reconstructedOperation.Context["traceparent"] != "00-abc" {
		t.Fatalf("traceparent context = %q, want 00-abc", reconstructedOperation.Context["traceparent"])
	}
	if len(reconstructedOperation.Commands) != 1 {
		t.Fatalf("Commands length = %d, want 1", len(reconstructedOperation.Commands))
	}
	if reconstructedOperation.Commands[0].Name != "GET" {
		t.Fatalf("command name = %q, want GET", reconstructedOperation.Commands[0].Name)
	}
	if reconstructedOperation.Status != "completed" {
		t.Fatalf("Status = %q, want completed", reconstructedOperation.Status)
	}

	select {
	case consumedOffset := <-ackOffsets:
		if consumedOffset != expectedConsumedOffset {
			t.Fatalf("ack consumed offset = %d, want %d", consumedOffset, expectedConsumedOffset)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for threshold telemetry ack")
	}

	select {
	case <-confirmations:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for telemetry ack confirmation")
	}
}

func TestTelemetryTmpfsCompactionPreservesUnreadOperation(t *testing.T) {
	writer := newTelemetryTmpfsIntegrationWriter(t)
	largePayload := bytes.Repeat([]byte{'x'}, commonobs.TmpfsCompactionThreshold+1024)
	writeTelemetryTmpfsOperation(t, writer, &gcpcv1.TelemetryOperation{
		OperationId: "discard-before-compaction",
		TelemetryItems: []*gcpcv1.TelemetryItem{
			newTelemetryTmpfsTestItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_LOG, largePayload),
		},
	})
	writeTelemetryTmpfsOperation(t, writer, &gcpcv1.TelemetryOperation{
		OperationId: "after-compact",
		InitialContext: []*gcpcv1.Tag{
			{Key: []byte("tenant"), Value: []byte("acme")},
		},
		TelemetryItems: []*gcpcv1.TelemetryItem{
			newTelemetryTmpfsTestItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH, encodeTelemetryTmpfsPairs(t,
				telemetryTestFieldStatus, "completed",
			)),
		},
	})
	compactions := make(chan bool, 4)

	telemetryPlugin := pluginsdk.NewTelemetryPlugin(
		writer.FilePath(),
		func(consumedOffset uint64) {
			writer.Acknowledge(consumedOffset)
		},
		func() error {
			compacted, err := writer.CompactIfNeeded()
			if err != nil {
				return err
			}
			compactions <- compacted
			return nil
		},
	).WithPollInterval(time.Millisecond).WithThresholds(1, time.Hour)
	startTelemetryTmpfsPlugin(t, telemetryPlugin)

	select {
	case compacted := <-compactions:
		if !compacted {
			t.Fatal("first CompactIfNeeded() compacted = false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for compaction confirmation")
	}

	reconstructedOperation := waitForTelemetryTmpfsOperation(t, telemetryPlugin.Operations())
	if reconstructedOperation.OperationID != "after-compact" {
		t.Fatalf("OperationID = %q, want after-compact", reconstructedOperation.OperationID)
	}
	if reconstructedOperation.Context["tenant"] != "acme" {
		t.Fatalf("tenant context = %q, want acme", reconstructedOperation.Context["tenant"])
	}
	if reconstructedOperation.Status != "completed" {
		t.Fatalf("Status = %q, want completed", reconstructedOperation.Status)
	}
}

func TestTelemetryTmpfsOverflowIncrementsCounter(t *testing.T) {
	writer := newTelemetryTmpfsIntegrationWriter(t)
	payloadCapacity := commonobs.TmpfsTelemetryFileSize - commonobs.TmpfsTelemetryHeaderSize
	fillPayload := bytes.Repeat([]byte{'f'}, payloadCapacity-telemetryTmpfsLengthPrefixSize)

	if _, err := writer.Write(fillPayload); err != nil {
		t.Fatalf("Write(fillPayload) error = %v", err)
	}
	if _, err := writer.Write([]byte("x")); !errors.Is(err, commonobs.ErrTmpfsTelemetryOverflow) {
		t.Fatalf("Write(overflow payload) error = %v, want %v", err, commonobs.ErrTmpfsTelemetryOverflow)
	}
	if overflowDropped := writer.OverflowDropped(); overflowDropped != 1 {
		t.Fatalf("OverflowDropped() = %d, want 1", overflowDropped)
	}
}

func TestTelemetryTmpfsAckRoundTripUpdatesWriterAndConfirms(t *testing.T) {
	writer := newTelemetryTmpfsIntegrationWriter(t)
	expectedConsumedOffset := writeTelemetryTmpfsOperation(t, writer, &gcpcv1.TelemetryOperation{
		OperationId: "ack-round-trip",
		TelemetryItems: []*gcpcv1.TelemetryItem{
			newTelemetryTmpfsTestItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH, encodeTelemetryTmpfsPairs(t,
				telemetryTestFieldStatus, "completed",
			)),
		},
	})
	ackOffsets := make(chan uint64, 2)
	confirmations := make(chan bool, 2)

	telemetryPlugin := pluginsdk.NewTelemetryPlugin(
		writer.FilePath(),
		func(consumedOffset uint64) {
			writer.Acknowledge(consumedOffset)
			ackOffsets <- consumedOffset
		},
		func() error {
			compacted, err := writer.CompactIfNeeded()
			if err != nil {
				return err
			}
			confirmations <- compacted
			return nil
		},
	).WithPollInterval(time.Millisecond).WithThresholds(1, time.Hour)
	startTelemetryTmpfsPlugin(t, telemetryPlugin)
	_ = waitForTelemetryTmpfsOperation(t, telemetryPlugin.Operations())

	select {
	case consumedOffset := <-ackOffsets:
		if consumedOffset != expectedConsumedOffset {
			t.Fatalf("ack consumed offset = %d, want %d", consumedOffset, expectedConsumedOffset)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for telemetry ack")
	}
	if writer.ConsumedOffset() != expectedConsumedOffset {
		t.Fatalf("ConsumedOffset() = %d, want %d", writer.ConsumedOffset(), expectedConsumedOffset)
	}

	select {
	case compacted := <-confirmations:
		if compacted {
			t.Fatal("CompactIfNeeded() compacted = true, want false below threshold")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ack confirmation")
	}
}

func newTelemetryTmpfsIntegrationWriter(t *testing.T) *commonobs.TmpfsTelemetryWriter {
	t.Helper()
	if _, err := os.Stat(commonobs.TmpfsTelemetryDir); err != nil {
		t.Skipf("tmpfs telemetry directory unavailable: %v", err)
	}
	writer, err := commonobs.NewTmpfsTelemetryWriter(fmt.Sprintf("integration-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("NewTmpfsTelemetryWriter() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := writer.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	})
	return writer
}

func writeTelemetryTmpfsOperation(t *testing.T, writer *commonobs.TmpfsTelemetryWriter, operation *gcpcv1.TelemetryOperation) uint64 {
	t.Helper()
	operationBytes, err := operation.MarshalVT()
	if err != nil {
		t.Fatalf("marshal telemetry operation: %v", err)
	}
	writeOffset := writer.WriteOffset()
	written, err := writer.Write(operationBytes)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if written != len(operationBytes) {
		t.Fatalf("Write() written = %d, want %d", written, len(operationBytes))
	}
	return writeOffset + uint64(telemetryTmpfsLengthPrefixSize+written)
}

func startTelemetryTmpfsPlugin(t *testing.T, telemetryPlugin *pluginsdk.TelemetryPlugin) {
	t.Helper()
	if err := telemetryPlugin.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := telemetryPlugin.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	})
}

func waitForTelemetryTmpfsOperation(t *testing.T, operations <-chan *pluginsdk.ReconstructedOperation) *pluginsdk.ReconstructedOperation {
	t.Helper()
	select {
	case reconstructedOperation, ok := <-operations:
		if !ok {
			t.Fatal("telemetry operation channel closed")
		}
		return reconstructedOperation
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconstructed telemetry operation")
	}
	return nil
}

func newTelemetryTmpfsTestItem(kind gcpcv1.TelemetryItemKind, payload []byte) *gcpcv1.TelemetryItem {
	return &gcpcv1.TelemetryItem{Kind: kind, Payload: payload}
}

func encodeTelemetryTmpfsPairs(t *testing.T, pairs ...string) []byte {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatalf("encodeTelemetryTmpfsPairs requires even key/value entries, got %d", len(pairs))
	}
	encodedPayload := make([]byte, 1)
	for pairIndex := 0; pairIndex+1 < len(pairs); pairIndex += 2 {
		pairKey := pairs[pairIndex]
		pairText := pairs[pairIndex+1]
		if len(pairKey) > 255 || len(pairText) > 255 {
			t.Fatalf("pair %q=%q exceeds one-byte length", pairKey, pairText)
		}
		encodedPayload = append(encodedPayload, byte(len(pairKey)))
		encodedPayload = append(encodedPayload, pairKey...)
		encodedPayload = append(encodedPayload, byte(len(pairText)))
		encodedPayload = append(encodedPayload, pairText...)
		encodedPayload[0]++
	}
	return encodedPayload
}
