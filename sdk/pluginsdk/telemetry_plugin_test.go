package pluginsdk

import (
	"encoding/binary"
	"os"
	"testing"
	"time"

	gcpcv1 "gocache/api/gcpc/v1"
)

func TestTelemetryPluginPollsAndPublishesReconstructedOperation(t *testing.T) {
	telemetryFilePath := newTelemetryPluginTestFile(t)
	operation := &gcpcv1.TelemetryOperation{
		OperationId: "op-telemetry-1",
		InitialContext: []*gcpcv1.Tag{
			{Key: []byte("tenant"), Value: []byte("acme")},
		},
		TelemetryItems: []*gcpcv1.TelemetryItem{
			telemetryPluginItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_CONTEXT_UPDATE, encodeTelemetryPairs(t,
				"trace", "abc",
			)),
			telemetryPluginItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH, encodeTelemetryPairs(t,
				telemetryFieldStatus, "completed",
			)),
		},
	}
	writeTelemetryPluginFrame(t, telemetryFilePath, operation, 0)

	telemetryPlugin := NewTelemetryPlugin(telemetryFilePath, nil, nil).WithPollInterval(time.Millisecond)
	if err := telemetryPlugin.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := telemetryPlugin.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	})

	select {
	case reconstructedOperation := <-telemetryPlugin.Operations():
		if reconstructedOperation.OperationID != "op-telemetry-1" {
			t.Fatalf("OperationID = %q, want op-telemetry-1", reconstructedOperation.OperationID)
		}
		if reconstructedOperation.Context["tenant"] != "acme" {
			t.Fatalf("tenant context = %q, want acme", reconstructedOperation.Context["tenant"])
		}
		if reconstructedOperation.Context["trace"] != "abc" {
			t.Fatalf("trace context = %q, want abc", reconstructedOperation.Context["trace"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconstructed telemetry operation")
	}
}

func TestTelemetryPluginSendsAckAfterByteThreshold(t *testing.T) {
	telemetryFilePath := newTelemetryPluginTestFile(t)
	operation := &gcpcv1.TelemetryOperation{
		OperationId: "op-telemetry-ack",
		TelemetryItems: []*gcpcv1.TelemetryItem{
			telemetryPluginItem(gcpcv1.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH, encodeTelemetryPairs(t,
				telemetryFieldStatus, "completed",
			)),
		},
	}
	expectedConsumedOffset := writeTelemetryPluginFrame(t, telemetryFilePath, operation, 0)
	ackOffsets := make(chan uint64, 1)
	confirmations := make(chan struct{}, 1)

	telemetryPlugin := NewTelemetryPlugin(
		telemetryFilePath,
		func(consumedOffset uint64) {
			writeTelemetryPluginHeader(t, telemetryFilePath, expectedConsumedOffset, consumedOffset)
			ackOffsets <- consumedOffset
		},
		func() error {
			confirmations <- struct{}{}
			return nil
		},
	).WithPollInterval(time.Millisecond).WithThresholds(1, time.Hour)
	if err := telemetryPlugin.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := telemetryPlugin.Stop(); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	})

	select {
	case reconstructedOperation := <-telemetryPlugin.Operations():
		if reconstructedOperation.OperationID != "op-telemetry-ack" {
			t.Fatalf("OperationID = %q, want op-telemetry-ack", reconstructedOperation.OperationID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconstructed telemetry operation")
	}

	select {
	case consumedOffset := <-ackOffsets:
		if consumedOffset != expectedConsumedOffset {
			t.Fatalf("ack consumed offset = %d, want %d", consumedOffset, expectedConsumedOffset)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for telemetry ack")
	}

	select {
	case <-confirmations:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ack confirmation callback")
	}
}

func newTelemetryPluginTestFile(t *testing.T) string {
	t.Helper()
	telemetryFile, err := os.CreateTemp(t.TempDir(), "telemetry-*.tmp")
	if err != nil {
		t.Fatalf("create telemetry test file: %v", err)
	}
	telemetryFilePath := telemetryFile.Name()
	if err := telemetryFile.Truncate(TmpfsHeaderSize); err != nil {
		t.Fatalf("truncate telemetry test file: %v", err)
	}
	if err := telemetryFile.Close(); err != nil {
		t.Fatalf("close telemetry test file: %v", err)
	}
	writeTelemetryPluginHeader(t, telemetryFilePath, 0, 0)
	return telemetryFilePath
}

func writeTelemetryPluginFrame(t *testing.T, telemetryFilePath string, operation *gcpcv1.TelemetryOperation, currentWriteOffset uint64) uint64 {
	t.Helper()
	operationBytes, err := operation.MarshalVT()
	if err != nil {
		t.Fatalf("marshal telemetry operation: %v", err)
	}
	telemetryFile, err := os.OpenFile(telemetryFilePath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open telemetry test file: %v", err)
	}
	defer func() {
		if err := telemetryFile.Close(); err != nil {
			t.Fatalf("close telemetry test file: %v", err)
		}
	}()

	lengthPrefix := make([]byte, tmpfsLengthPrefixSize)
	binary.BigEndian.PutUint32(lengthPrefix, uint32(len(operationBytes)))
	frameOffset := int64(TmpfsHeaderSize + currentWriteOffset)
	if _, err := telemetryFile.WriteAt(lengthPrefix, frameOffset); err != nil {
		t.Fatalf("write telemetry length prefix: %v", err)
	}
	if _, err := telemetryFile.WriteAt(operationBytes, frameOffset+tmpfsLengthPrefixSize); err != nil {
		t.Fatalf("write telemetry operation bytes: %v", err)
	}
	nextWriteOffset := currentWriteOffset + uint64(tmpfsLengthPrefixSize+len(operationBytes))
	writeTelemetryPluginHeader(t, telemetryFilePath, nextWriteOffset, 0)
	return nextWriteOffset
}

func writeTelemetryPluginHeader(t *testing.T, telemetryFilePath string, writeOffset, consumedOffset uint64) {
	t.Helper()
	headerBytes := make([]byte, TmpfsHeaderSize)
	binary.LittleEndian.PutUint64(headerBytes[tmpfsHeaderSeqStart:tmpfsHeaderWriteStart], 2)
	binary.LittleEndian.PutUint64(headerBytes[tmpfsHeaderWriteStart:tmpfsHeaderReadStart], writeOffset)
	binary.LittleEndian.PutUint64(headerBytes[tmpfsHeaderReadStart:TmpfsHeaderSize], consumedOffset)
	telemetryFile, err := os.OpenFile(telemetryFilePath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open telemetry test file for header write: %v", err)
	}
	defer func() {
		if err := telemetryFile.Close(); err != nil {
			t.Fatalf("close telemetry test file after header write: %v", err)
		}
	}()
	if _, err := telemetryFile.WriteAt(headerBytes, 0); err != nil {
		t.Fatalf("write telemetry header: %v", err)
	}
}

func telemetryPluginItem(kind gcpcv1.TelemetryItemKind, payload []byte) *gcpcv1.TelemetryItem {
	return &gcpcv1.TelemetryItem{Kind: kind, Payload: payload}
}
