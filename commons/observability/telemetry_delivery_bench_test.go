//go:build linux

package observability

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"
)

type tmpfsTelemetryBenchmarkReader struct {
	file         *os.File
	headerBytes  [TmpfsTelemetryHeaderSize]byte
	lengthPrefix [tmpfsTelemetryLengthPrefixSize]byte
	messageBytes []byte
	readOffset   uint64
}

func openTmpfsTelemetryBenchmarkReader(b *testing.B, pluginName string, payloadSize int) *tmpfsTelemetryBenchmarkReader {
	b.Helper()

	filePath, err := tmpfsTelemetryFilePath(pluginName)
	if err != nil {
		b.Fatalf("tmpfsTelemetryFilePath() error = %v", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		b.Fatalf("open tmpfs telemetry benchmark reader: %v", err)
	}
	b.Cleanup(func() {
		if closeErr := file.Close(); closeErr != nil {
			b.Fatalf("close tmpfs telemetry benchmark reader: %v", closeErr)
		}
	})

	return &tmpfsTelemetryBenchmarkReader{
		file:         file,
		messageBytes: make([]byte, payloadSize),
	}
}

func (r *tmpfsTelemetryBenchmarkReader) readNext(writer *TmpfsTelemetryWriter) error {
	for {
		if _, err := r.file.ReadAt(r.headerBytes[:], 0); err != nil {
			return fmt.Errorf("read tmpfs telemetry header: %w", err)
		}

		initialSequence := binary.LittleEndian.Uint64(r.headerBytes[tmpfsTelemetryHeaderSequenceOffset:tmpfsTelemetryHeaderWriteOffset])
		if initialSequence%2 != 0 {
			continue
		}

		writeOffset := binary.LittleEndian.Uint64(r.headerBytes[tmpfsTelemetryHeaderWriteOffset:tmpfsTelemetryHeaderConsumedOffset])
		if _, err := r.file.ReadAt(r.headerBytes[tmpfsTelemetryHeaderSequenceOffset:tmpfsTelemetryHeaderWriteOffset], 0); err != nil {
			return fmt.Errorf("reread tmpfs telemetry header sequence: %w", err)
		}
		confirmedSequence := binary.LittleEndian.Uint64(r.headerBytes[tmpfsTelemetryHeaderSequenceOffset:tmpfsTelemetryHeaderWriteOffset])
		if initialSequence != confirmedSequence {
			continue
		}

		if writeOffset < r.readOffset {
			r.readOffset = 0
		}
		if writeOffset-r.readOffset < tmpfsTelemetryLengthPrefixSize {
			continue
		}

		lengthPosition := int64(TmpfsTelemetryHeaderSize + r.readOffset)
		if _, err := r.file.ReadAt(r.lengthPrefix[:], lengthPosition); err != nil {
			return fmt.Errorf("read tmpfs telemetry length prefix: %w", err)
		}

		messageLength := uint64(binary.BigEndian.Uint32(r.lengthPrefix[:]))
		entrySize := uint64(tmpfsTelemetryLengthPrefixSize) + messageLength
		if writeOffset-r.readOffset < entrySize {
			continue
		}
		if messageLength > uint64(cap(r.messageBytes)) {
			r.messageBytes = make([]byte, messageLength)
		}
		messageBuffer := r.messageBytes[:messageLength]
		messagePosition := lengthPosition + tmpfsTelemetryLengthPrefixSize
		if _, err := r.file.ReadAt(messageBuffer, messagePosition); err != nil {
			return fmt.Errorf("read tmpfs telemetry payload: %w", err)
		}

		r.readOffset += entrySize
		writer.Acknowledge(r.readOffset)
		return nil
	}
}

func BenchmarkTelemetryDeliveryLatency_Tmpfs(b *testing.B) {
	if _, err := os.Stat(TmpfsTelemetryDir); err != nil {
		b.Skipf("tmpfs telemetry directory unavailable: %v", err)
	}

	pluginName := fmt.Sprintf("bench-tmpfs-%s", b.Name())
	writer, err := NewTmpfsTelemetryWriter(pluginName)
	if err != nil {
		b.Fatalf("NewTmpfsTelemetryWriter() error = %v", err)
	}
	b.Cleanup(func() {
		if closeErr := writer.Close(); closeErr != nil {
			b.Fatalf("Close() error = %v", closeErr)
		}
	})

	const telemetryPayloadSize = 128
	payload := make([]byte, telemetryPayloadSize)
	reader := openTmpfsTelemetryBenchmarkReader(b, pluginName, telemetryPayloadSize)
	received := make(chan struct{}, 1)
	readErrors := make(chan error, 1)
	iterationCount := b.N

	go func() {
		for iteration := 0; iteration < iterationCount; iteration++ {
			if err := reader.readNext(writer); err != nil {
				readErrors <- err
				return
			}
			received <- struct{}{}
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := writer.Write(payload); err != nil {
			b.Fatalf("Write() error = %v", err)
		}

		select {
		case <-received:
		case err := <-readErrors:
			b.Fatalf("tmpfs telemetry benchmark reader error: %v", err)
		}
	}
}
