//go:build linux

package observability

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode"
	"unsafe"
)

const (
	TmpfsTelemetryFileSize   = 15 * 1024 * 1024
	TmpfsTelemetryHeaderSize = 24
	TmpfsTelemetryDir        = "/dev/shm"
	TmpfsTelemetryPrefix     = "gocache-telemetry-"
	TmpfsCompactionThreshold = 14 * 1024 * 1024
)

const (
	tmpfsTelemetryHeaderSequenceOffset = 0
	tmpfsTelemetryHeaderWriteOffset    = 8
	tmpfsTelemetryHeaderConsumedOffset = 16
)

const tmpfsTelemetryLengthPrefixSize = 4

var (
	ErrTmpfsTelemetryPayloadTooLarge = errors.New("tmpfs telemetry payload too large")
	ErrTmpfsTelemetryOverflow        = errors.New("tmpfs telemetry buffer overflow")
	ErrTmpfsTelemetryClosed          = errors.New("tmpfs telemetry writer closed")
	ErrTmpfsTelemetryPluginNameEmpty = errors.New("tmpfs telemetry plugin name is empty")
)

type TmpfsTelemetryWriter struct {
	mu              sync.Mutex
	file            *os.File
	data            []byte
	headerSeq       uint64
	writeOffset     uint64
	consumedOffset  uint64
	pluginName      string
	filePath        string
	overflowDropped uint64
}

func NewTmpfsTelemetryWriter(pluginName string) (*TmpfsTelemetryWriter, error) {
	filePath, err := tmpfsTelemetryFilePath(pluginName)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open tmpfs telemetry file: %w", err)
	}

	if err := syscall.Ftruncate(int(file.Fd()), TmpfsTelemetryFileSize); err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(filePath)
		return nil, errors.Join(fmt.Errorf("preallocate tmpfs telemetry file: %w", err), closeErr, removeErr)
	}

	mappedData, err := syscall.Mmap(int(file.Fd()), 0, TmpfsTelemetryFileSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		closeErr := file.Close()
		removeErr := os.Remove(filePath)
		return nil, errors.Join(fmt.Errorf("mmap tmpfs telemetry file: %w", err), closeErr, removeErr)
	}

	writer := &TmpfsTelemetryWriter{
		file:       file,
		data:       mappedData,
		pluginName: pluginName,
		filePath:   filePath,
	}
	writer.storeHeaderLocked(0, 0)
	return writer, nil
}

func (w *TmpfsTelemetryWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closedLocked() {
		return 0, ErrTmpfsTelemetryClosed
	}
	entrySize, err := tmpfsTelemetryEntrySize(data)
	if err != nil {
		w.overflowDropped++
		return 0, err
	}
	if w.writeOffset+entrySize > tmpfsTelemetryPayloadCapacity() {
		if _, err := w.compactLocked(true); err != nil {
			return 0, err
		}
	}
	if w.writeOffset+entrySize > tmpfsTelemetryPayloadCapacity() {
		w.overflowDropped++
		return 0, ErrTmpfsTelemetryOverflow
	}

	writeStart := TmpfsTelemetryHeaderSize + int(w.writeOffset)
	var lengthPrefix [4]byte
	binary.BigEndian.PutUint32(lengthPrefix[:], uint32(len(data)))
	copy(w.data[writeStart:writeStart+tmpfsTelemetryLengthPrefixSize], lengthPrefix[:])
	copy(w.data[writeStart+tmpfsTelemetryLengthPrefixSize:writeStart+int(entrySize)], data)

	w.writeOffset += entrySize
	w.storeWriteOffsetLocked(w.writeOffset)
	if w.writeOffset >= TmpfsCompactionThreshold {
		if _, err := w.compactLocked(false); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (w *TmpfsTelemetryWriter) Acknowledge(consumedOffset uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closedLocked() {
		return
	}
	if consumedOffset < w.consumedOffset {
		return
	}
	if consumedOffset > w.writeOffset {
		consumedOffset = w.writeOffset
	}
	w.consumedOffset = consumedOffset
	w.storeConsumedOffsetLocked(consumedOffset)
}

func (w *TmpfsTelemetryWriter) CompactIfNeeded() (compacted bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closedLocked() {
		return false, ErrTmpfsTelemetryClosed
	}
	return w.compactLocked(false)
}

func (w *TmpfsTelemetryWriter) OverflowDropped() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overflowDropped
}

func (w *TmpfsTelemetryWriter) Close() error {
	w.mu.Lock()
	mappedData := w.data
	file := w.
		file
	filePath := w.filePath
	w.data = nil
	w.file = nil
	w.filePath = ""
	w.writeOffset = 0
	w.consumedOffset = 0
	w.mu.Unlock()

	var munmapErr error
	if mappedData != nil {
		munmapErr = syscall.Munmap(mappedData)
	}
	var closeErr error
	if file != nil {
		closeErr = file.Close()
	}
	var removeErr error
	if filePath != "" {
		removeErr = os.Remove(filePath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
	}
	return errors.Join(munmapErr, closeErr, removeErr)
}

func (w *TmpfsTelemetryWriter) FilePath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.filePath
}

func (w *TmpfsTelemetryWriter) WriteOffset() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeOffset
}

func (w *TmpfsTelemetryWriter) ConsumedOffset() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.consumedOffset
}

func (w *TmpfsTelemetryWriter) ReadHeader() (writeOffset, consumedOffset uint64, err error) {
	for {
		if w.closedLocked() {
			return 0, 0, ErrTmpfsTelemetryClosed
		}

		initialSequence := atomic.LoadUint64(tmpfsTelemetryHeaderField(w.data, tmpfsTelemetryHeaderSequenceOffset))
		if initialSequence%2 != 0 {
			continue
		}

		currentWriteOffset := atomic.LoadUint64(tmpfsTelemetryHeaderField(w.data, tmpfsTelemetryHeaderWriteOffset))
		currentConsumedOffset := atomic.LoadUint64(tmpfsTelemetryHeaderField(w.data, tmpfsTelemetryHeaderConsumedOffset))
		confirmedSequence := atomic.LoadUint64(tmpfsTelemetryHeaderField(w.data, tmpfsTelemetryHeaderSequenceOffset))
		if initialSequence == confirmedSequence {
			return currentWriteOffset, currentConsumedOffset, nil
		}
	}
}

func (w *TmpfsTelemetryWriter) compactLocked(force bool) (bool, error) {
	if !force && w.writeOffset < TmpfsCompactionThreshold {
		return false, nil
	}
	if w.consumedOffset == 0 {
		return false, nil
	}
	if w.consumedOffset > w.writeOffset {
		w.consumedOffset = w.writeOffset
	}

	remainingBytes := w.writeOffset - w.consumedOffset
	sourceStart := TmpfsTelemetryHeaderSize + int(w.consumedOffset)
	sourceEnd := TmpfsTelemetryHeaderSize + int(w.writeOffset)
	destinationEnd := TmpfsTelemetryHeaderSize + int(remainingBytes)
	copy(w.data[TmpfsTelemetryHeaderSize:destinationEnd], w.data[sourceStart:sourceEnd])
	clear(w.data[destinationEnd:sourceEnd])
	w.writeOffset = remainingBytes
	w.consumedOffset = 0
	w.storeHeaderLocked(w.writeOffset, 0)
	return true, nil
}

func (w *TmpfsTelemetryWriter) storeHeaderLocked(writeOffset, consumedOffset uint64) {
	w.headerSeq++
	atomic.StoreUint64(tmpfsTelemetryHeaderField(w.data, tmpfsTelemetryHeaderSequenceOffset), w.headerSeq)
	atomic.StoreUint64(tmpfsTelemetryHeaderField(w.data, tmpfsTelemetryHeaderWriteOffset), writeOffset)
	atomic.StoreUint64(tmpfsTelemetryHeaderField(w.data, tmpfsTelemetryHeaderConsumedOffset), consumedOffset)
	w.headerSeq++
	atomic.StoreUint64(tmpfsTelemetryHeaderField(w.data, tmpfsTelemetryHeaderSequenceOffset), w.headerSeq)
}

func (w *TmpfsTelemetryWriter) storeWriteOffsetLocked(writeOffset uint64) {
	w.storeHeaderLocked(writeOffset, w.consumedOffset)
}

func (w *TmpfsTelemetryWriter) storeConsumedOffsetLocked(consumedOffset uint64) {
	w.storeHeaderLocked(w.writeOffset, consumedOffset)
}

func tmpfsTelemetryHeaderField(data []byte, offset int) *uint64 {
	return (*uint64)(unsafe.Pointer(&data[offset]))
}

func (w *TmpfsTelemetryWriter) closedLocked() bool {
	return w.file == nil || w.data == nil
}

func tmpfsTelemetryEntrySize(data []byte) (uint64, error) {
	payloadLength := uint64(len(data))
	if payloadLength > tmpfsTelemetryPayloadCapacity()-tmpfsTelemetryLengthPrefixSize {
		return 0, ErrTmpfsTelemetryPayloadTooLarge
	}
	return uint64(tmpfsTelemetryLengthPrefixSize) + payloadLength, nil
}

func tmpfsTelemetryPayloadCapacity() uint64 {
	return TmpfsTelemetryFileSize - TmpfsTelemetryHeaderSize
}

func tmpfsTelemetryFilePath(pluginName string) (string, error) {
	safePluginName := sanitizeTmpfsTelemetryPluginName(pluginName)
	if safePluginName == "" {
		return "", ErrTmpfsTelemetryPluginNameEmpty
	}
	return filepath.Join(TmpfsTelemetryDir, TmpfsTelemetryPrefix+safePluginName), nil
}

func sanitizeTmpfsTelemetryPluginName(pluginName string) string {
	trimmedPluginName := strings.TrimSpace(pluginName)
	return strings.Map(func(char rune) rune {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '-' || char == '_' || char == '.' {
			return char
		}
		return '_'
	}, trimmedPluginName)
}
