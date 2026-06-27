//go:build linux

package observability

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func newTestTmpfsTelemetryWriter(t *testing.T) *TmpfsTelemetryWriter {
	t.Helper()
	if _, statErr := os.Stat(TmpfsTelemetryDir); statErr != nil {
		t.Skipf("tmpfs telemetry directory unavailable: %v", statErr)
	}
	writer, err := NewTmpfsTelemetryWriter(fmt.Sprintf("test-%d", time.Now().UnixNano()))
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

func mapTmpfsTelemetryFile(t *testing.T, filePath string) []byte {
	t.Helper()
	file, err := os.OpenFile(filePath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open tmpfs telemetry file for mmap: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Fatalf("close tmpfs telemetry mmap file: %v", closeErr)
		}
	})
	mappedData, err := syscall.Mmap(int(file.Fd()), 0, TmpfsTelemetryFileSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap tmpfs telemetry file: %v", err)
	}
	t.Cleanup(func() {
		if munmapErr := syscall.Munmap(mappedData); munmapErr != nil {
			t.Fatalf("munmap tmpfs telemetry file: %v", munmapErr)
		}
	})
	return mappedData
}

func TestTmpfsTelemetryWriterCreateInitializesFileAndHeader(t *testing.T) {
	writer := newTestTmpfsTelemetryWriter(t)
	filePath := writer.FilePath()

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat tmpfs telemetry file: %v", err)
	}
	if fileInfo.Size() != TmpfsTelemetryFileSize {
		t.Fatalf("tmpfs telemetry file size = %d, want %d", fileInfo.Size(), TmpfsTelemetryFileSize)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("file perm = %o, want 0o600", fileInfo.Mode().Perm())
	}

	mappedData := mapTmpfsTelemetryFile(t, filePath)
	if TmpfsTelemetryHeaderSize != 24 {
		t.Fatalf("TmpfsTelemetryHeaderSize = %d, want 24", TmpfsTelemetryHeaderSize)
	}
	if sequenceCounter := binary.LittleEndian.Uint64(mappedData[0:8]); sequenceCounter%2 != 0 {
		t.Fatalf("header sequence counter = %d, want even", sequenceCounter)
	}
	if writeOffset := binary.LittleEndian.Uint64(mappedData[8:16]); writeOffset != 0 {
		t.Fatalf("header write offset = %d, want 0", writeOffset)
	}
	if consumedOffset := binary.LittleEndian.Uint64(mappedData[16:24]); consumedOffset != 0 {
		t.Fatalf("header consumed offset = %d, want 0", consumedOffset)
	}
}

func TestTmpfsTelemetryWriterWriteFramesAndAcknowledges(t *testing.T) {
	writer := newTestTmpfsTelemetryWriter(t)
	filePath := writer.FilePath()
	payload := []byte("encoded protobuf payload")

	written, err := writer.Write(payload)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if written != len(payload) {
		t.Fatalf("Write() written = %d, want %d", written, len(payload))
	}

	entrySize := uint64(tmpfsTelemetryLengthPrefixSize + len(payload))
	if currentWriteOffset := writer.WriteOffset(); currentWriteOffset != entrySize {
		t.Fatalf("WriteOffset() = %d, want %d", currentWriteOffset, entrySize)
	}
	mappedData := mapTmpfsTelemetryFile(t, filePath)
	if headerWriteOffset := binary.LittleEndian.Uint64(mappedData[8:16]); headerWriteOffset != entrySize {
		t.Fatalf("header write offset = %d, want %d", headerWriteOffset, entrySize)
	}
	if frameLength := binary.BigEndian.Uint32(mappedData[TmpfsTelemetryHeaderSize : TmpfsTelemetryHeaderSize+tmpfsTelemetryLengthPrefixSize]); frameLength != uint32(len(payload)) {
		t.Fatalf("frame length = %d, want %d", frameLength, len(payload))
	}
	frameStart := TmpfsTelemetryHeaderSize + tmpfsTelemetryLengthPrefixSize
	frameEnd := frameStart + len(payload)
	if !bytes.Equal(mappedData[frameStart:frameEnd], payload) {
		t.Fatalf("frame payload = %q, want %q", mappedData[frameStart:frameEnd], payload)
	}

	writer.Acknowledge(entrySize)
	if consumedOffset := writer.ConsumedOffset(); consumedOffset != entrySize {
		t.Fatalf("ConsumedOffset() = %d, want %d", consumedOffset, entrySize)
	}
	if headerConsumedOffset := binary.LittleEndian.Uint64(mappedData[16:24]); headerConsumedOffset != entrySize {
		t.Fatalf("header consumed offset = %d, want %d", headerConsumedOffset, entrySize)
	}
}

func TestTmpfsTelemetryWriterCompactsUnconsumedData(t *testing.T) {
	writer := newTestTmpfsTelemetryWriter(t)
	filePath := writer.FilePath()
	discardPayload := bytes.Repeat([]byte{'d'}, TmpfsCompactionThreshold-tmpfsTelemetryLengthPrefixSize)
	keepPayload := []byte("keep")

	if _, err := writer.Write(discardPayload); err != nil {
		t.Fatalf("Write(discardPayload) error = %v", err)
	}
	keepOffset := writer.WriteOffset()
	written, err := writer.Write(keepPayload)
	if err != nil {
		t.Fatalf("Write(keepPayload) error = %v", err)
	}
	if keepOffset != TmpfsCompactionThreshold || written != len(keepPayload) {
		t.Fatalf("keep frame offset/written = %d/%d, want %d/%d", keepOffset, written, TmpfsCompactionThreshold, len(keepPayload))
	}

	writer.Acknowledge(TmpfsCompactionThreshold)
	compacted, err := writer.CompactIfNeeded()
	if err != nil {
		t.Fatalf("CompactIfNeeded() error = %v", err)
	}
	if !compacted {
		t.Fatal("CompactIfNeeded() compacted = false, want true")
	}

	keepEntrySize := uint64(tmpfsTelemetryLengthPrefixSize + len(keepPayload))
	if writeOffset := writer.WriteOffset(); writeOffset != keepEntrySize {
		t.Fatalf("WriteOffset() after compaction = %d, want %d", writeOffset, keepEntrySize)
	}
	if consumedOffset := writer.ConsumedOffset(); consumedOffset != 0 {
		t.Fatalf("ConsumedOffset() after compaction = %d, want 0", consumedOffset)
	}
	mappedData := mapTmpfsTelemetryFile(t, filePath)
	if headerWriteOffset := binary.LittleEndian.Uint64(mappedData[8:16]); headerWriteOffset != keepEntrySize {
		t.Fatalf("header write offset after compaction = %d, want %d", headerWriteOffset, keepEntrySize)
	}
	if frameLength := binary.BigEndian.Uint32(mappedData[TmpfsTelemetryHeaderSize : TmpfsTelemetryHeaderSize+tmpfsTelemetryLengthPrefixSize]); frameLength != uint32(len(keepPayload)) {
		t.Fatalf("compacted frame length = %d, want %d", frameLength, len(keepPayload))
	}
	frameStart := TmpfsTelemetryHeaderSize + tmpfsTelemetryLengthPrefixSize
	frameEnd := frameStart + len(keepPayload)
	if !bytes.Equal(mappedData[frameStart:frameEnd], keepPayload) {
		t.Fatalf("compacted payload = %q, want %q", mappedData[frameStart:frameEnd], keepPayload)
	}
}

func TestTmpfsTelemetryWriterReadHeaderRetriesDuringUpdate(t *testing.T) {
	writer := newTestTmpfsTelemetryWriter(t)
	const expectedWriteOffset = uint64(128)
	const expectedConsumedOffset = uint64(64)

	atomic.StoreUint64(tmpfsTelemetryHeaderField(writer.data, tmpfsTelemetryHeaderSequenceOffset), 3)
	atomic.StoreUint64(tmpfsTelemetryHeaderField(writer.data, tmpfsTelemetryHeaderWriteOffset), expectedWriteOffset)
	atomic.StoreUint64(tmpfsTelemetryHeaderField(writer.data, tmpfsTelemetryHeaderConsumedOffset), expectedConsumedOffset)

	type headerRead struct {
		writeOffset    uint64
		consumedOffset uint64
		err            error
	}
	readResults := make(chan headerRead, 1)
	go func() {
		writeOffset, consumedOffset, err := writer.ReadHeader()
		readResults <- headerRead{writeOffset: writeOffset, consumedOffset: consumedOffset, err: err}
	}()

	select {
	case header := <-readResults:
		t.Fatalf("ReadHeader() returned during odd sequence counter: %+v", header)
	case <-time.After(10 * time.Millisecond):
	}

	atomic.StoreUint64(tmpfsTelemetryHeaderField(writer.data, tmpfsTelemetryHeaderSequenceOffset), 4)
	select {
	case header := <-readResults:
		if header.err != nil {
			t.Fatalf("ReadHeader() error = %v", header.err)
		}
		if header.writeOffset != expectedWriteOffset || header.consumedOffset != expectedConsumedOffset {
			t.Fatalf("ReadHeader() = (%d, %d), want (%d, %d)", header.writeOffset, header.consumedOffset, expectedWriteOffset, expectedConsumedOffset)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadHeader() did not return after even sequence counter")
	}
}

func TestTmpfsTelemetryWriterOverflowIncrementsCounter(t *testing.T) {
	writer := newTestTmpfsTelemetryWriter(t)
	fillPayload := bytes.Repeat([]byte{'f'}, int(tmpfsTelemetryPayloadCapacity())-tmpfsTelemetryLengthPrefixSize)

	if _, err := writer.Write(fillPayload); err != nil {
		t.Fatalf("Write(fillPayload) error = %v", err)
	}
	if _, err := writer.Write([]byte("x")); !errors.Is(err, ErrTmpfsTelemetryOverflow) {
		t.Fatalf("Write(overflow payload) error = %v, want %v", err, ErrTmpfsTelemetryOverflow)
	}
	if overflowDropped := writer.OverflowDropped(); overflowDropped != 1 {
		t.Fatalf("OverflowDropped() = %d, want 1", overflowDropped)
	}
}
