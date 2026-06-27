package pluginsdk

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"time"

	gcpcv1 "gocache/api/gcpc/v1"
)

const (
	DefaultPollInterval  = 10 * time.Millisecond
	DefaultByteThreshold = 64 * 1024
	DefaultTimeThreshold = 5 * time.Minute
	TmpfsHeaderSize      = 24

	tmpfsLengthPrefixSize = 4
	tmpfsHeaderSeqStart   = 0
	tmpfsHeaderWriteStart = 8
	tmpfsHeaderReadStart  = 16
	operationBufferSize    = 1024
	telemetryHeaderRetries = 1000
)

// TelemetryPlugin handles tmpfs telemetry file reading, context reconstruction,
// threshold-based acking, and exposes a subscription channel for plugins.
type TelemetryPlugin struct {
	mu             sync.Mutex
	filePath       string
	file           *os.File
	pollInterval   time.Duration
	byteThreshold  int
	timeThreshold  time.Duration
	reconstructor  *ContextReconstructor
	op             chan *ReconstructedOperation
	opCloseOnce    sync.Once
	stopCh         chan struct{}
	doneCh         chan struct{}
	readOffset     uint64
	bytesSinceAck  int
	lastAckTime    time.Time
	ackFunc        func(consumedOffset uint64)
	waitForConfirm func() error
}

// NewTelemetryPlugin creates a TelemetryPlugin that reads from the given tmpfs file path.
// ackFunc sends TelemetryAck{consumed_offset} via the socket.
// waitForConfirm blocks until the server confirms compaction is done (then plugin resumes).
func NewTelemetryPlugin(filePath string, ackFunc func(uint64), waitForConfirm func() error) *TelemetryPlugin {
	return &TelemetryPlugin{
		filePath:       filePath,
		pollInterval:   DefaultPollInterval,
		byteThreshold:  DefaultByteThreshold,
		timeThreshold:  DefaultTimeThreshold,
		reconstructor:  NewContextReconstructor(),
		op:             make(chan *ReconstructedOperation, operationBufferSize),
		ackFunc:        ackFunc,
		waitForConfirm: waitForConfirm,
	}
}

// Start opens the tmpfs file and begins polling in a background goroutine.
func (t *TelemetryPlugin) Start() error {
	if t == nil {
		return fmt.Errorf("start telemetry plugin: nil receiver")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file != nil {
		return fmt.Errorf("start telemetry plugin: already started")
	}

	file, err := os.Open(t.filePath)
	if err != nil {
		return fmt.Errorf("open telemetry tmpfs file: %w", err)
	}

	writeOffset, consumedOffset, err := readTelemetryHeader(file, nil)
	if err != nil {
		closeErr := file.Close()
		return fmt.Errorf("read telemetry tmpfs header: %w", errOrJoin(err, closeErr))
	}

	t.file = file
	t.stopCh = make(chan struct{})
	t.doneCh = make(chan struct{})
	t.readOffset = consumedOffset
	if t.readOffset > writeOffset {
		t.readOffset = writeOffset
	}
	t.bytesSinceAck = 0
	t.lastAckTime = time.Now()

	go t.pollLoop(file, t.stopCh, t.doneCh)
	return nil
}

// Operations returns the subscription channel. Plugins receive ReconstructedOperations here.
func (t *TelemetryPlugin) Operations() <-chan *ReconstructedOperation {
	if t == nil {
		return nil
	}
	return t.op
}

// Stop closes the file and stops polling.
func (t *TelemetryPlugin) Stop() error {
	if t == nil {
		return nil
	}

	t.mu.Lock()
	file := t.file
	stopCh := t.stopCh
	doneCh := t.doneCh
	if file == nil {
		t.mu.Unlock()
		return nil
	}
	t.file = nil
	t.stopCh = nil
	t.doneCh = nil
	t.mu.Unlock()

	close(stopCh)
	<-doneCh
	if err := file.Close(); err != nil {
		return fmt.Errorf("close telemetry tmpfs file: %w", err)
	}
	return nil
}

// WithPollInterval configures the poll interval (default 10ms).
func (t *TelemetryPlugin) WithPollInterval(d time.Duration) *TelemetryPlugin {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if d > 0 {
		t.pollInterval = d
	}
	return t
}

// WithThresholds configures byte and time ack thresholds.
func (t *TelemetryPlugin) WithThresholds(byteThreshold int, timeThreshold time.Duration) *TelemetryPlugin {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if byteThreshold > 0 {
		t.byteThreshold = byteThreshold
	}
	if timeThreshold > 0 {
		t.timeThreshold = timeThreshold
	}
	return t
}

func (t *TelemetryPlugin) pollLoop(file *os.File, stopCh <-chan struct{}, doneCh chan<- struct{}) {
	defer close(doneCh)
	defer t.opCloseOnce.Do(func() {
		close(t.op)
	})

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		progress, err := t.pollOnce(file, stopCh)
		if err != nil {
			fmt.Fprintf(os.Stderr, "telemetry plugin poll error: %v\n", err)
		}
		if !progress {
			if !sleepOrStop(t.currentPollInterval(), stopCh) {
				return
			}
		}
	}
}

func (t *TelemetryPlugin) pollOnce(file *os.File, stopCh <-chan struct{}) (bool, error) {
	writeOffset, _, err := readTelemetryHeader(file, stopCh)
	if err != nil {
		return false, err
	}

	readOffset, bytesSinceAck, lastAckTime, byteThreshold, timeThreshold := t.currentReadState()
	if readOffset < writeOffset {
		advanced, err := t.readOperation(file, readOffset, writeOffset, stopCh)
		return advanced, err
	}

	if shouldSendTelemetryAck(bytesSinceAck, lastAckTime, byteThreshold, timeThreshold) {
		if err := t.sendAckAndWait(file, stopCh); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (t *TelemetryPlugin) readOperation(file *os.File, readOffset, writeOffset uint64, stopCh <-chan struct{}) (bool, error) {
	if writeOffset-readOffset < tmpfsLengthPrefixSize {
		return false, nil
	}

	lengthBuffer := make([]byte, tmpfsLengthPrefixSize)
	lengthPosition := int64(TmpfsHeaderSize + readOffset)
	if _, err := file.ReadAt(lengthBuffer, lengthPosition); err != nil {
		return false, fmt.Errorf("read telemetry length prefix: %w", err)
	}

	messageLength := uint64(binary.BigEndian.Uint32(lengthBuffer))
	entrySize := uint64(tmpfsLengthPrefixSize) + messageLength
	if writeOffset-readOffset < entrySize {
		return false, nil
	}

	messageBytes := make([]byte, messageLength)
	messagePosition := lengthPosition + tmpfsLengthPrefixSize
	if _, err := file.ReadAt(messageBytes, messagePosition); err != nil {
		return false, fmt.Errorf("read telemetry operation: %w", err)
	}

	operation := &gcpcv1.TelemetryOperation{}
	if err := operation.UnmarshalVT(messageBytes); err != nil {
		return false, fmt.Errorf("unmarshal telemetry operation at offset %d: %w", readOffset, err)
	}

	if reconstructedOperation := t.reconstructor.ProcessOperation(operation); reconstructedOperation != nil {
		select {
		case t.op <- reconstructedOperation:
		case <-stopCh:
			return false, nil
		}
	}

	t.advanceReadOffset(entrySize)
	if shouldSendTelemetryAck(t.currentBytesSinceAck(), t.currentLastAckTime(), t.currentByteThreshold(), t.currentTimeThreshold()) {
		if err := t.sendAckAndWait(file, stopCh); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (t *TelemetryPlugin) sendAckAndWait(file *os.File, stopCh <-chan struct{}) error {
	t.mu.Lock()
	consumedOffset := t.readOffset
	ackFunc := t.ackFunc
	waitForConfirm := t.waitForConfirm
	t.mu.Unlock()

	if ackFunc != nil {
		ackFunc(consumedOffset)
	}
	if waitForConfirm != nil {
		select {
		case <-stopCh:
			return nil
		default:
		}

		confirmErrCh := make(chan error, 1)
		go func() {
			confirmErrCh <- waitForConfirm()
		}()

		select {
		case <-stopCh:
			return nil
		case err := <-confirmErrCh:
			if err != nil {
				return fmt.Errorf("wait for telemetry ack confirmation: %w", err)
			}
		}
	}

	select {
	case <-stopCh:
		return nil
	default:
	}

	writeOffset, consumedHeaderOffset, err := readTelemetryHeader(file, stopCh)
	if err != nil {
		return fmt.Errorf("read telemetry header after ack: %w", err)
	}
	if consumedHeaderOffset > writeOffset {
		consumedHeaderOffset = writeOffset
	}

	t.mu.Lock()
	t.readOffset = consumedHeaderOffset
	t.bytesSinceAck = 0
	t.lastAckTime = time.Now()
	t.mu.Unlock()
	return nil
}

func (t *TelemetryPlugin) advanceReadOffset(entrySize uint64) {
	t.mu.Lock()
	t.readOffset += entrySize
	t.bytesSinceAck += int(entrySize)
	t.mu.Unlock()
}

func (t *TelemetryPlugin) currentReadState() (uint64, int, time.Time, int, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.readOffset, t.bytesSinceAck, t.lastAckTime, t.byteThreshold, t.timeThreshold
}

func (t *TelemetryPlugin) currentBytesSinceAck() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytesSinceAck
}

func (t *TelemetryPlugin) currentLastAckTime() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastAckTime
}

func (t *TelemetryPlugin) currentByteThreshold() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byteThreshold
}

func (t *TelemetryPlugin) currentTimeThreshold() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timeThreshold
}

func (t *TelemetryPlugin) currentPollInterval() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pollInterval
}

func readTelemetryHeader(file *os.File, stopCh <-chan struct{}) (uint64, uint64, error) {
	if file == nil {
		return 0, 0, fmt.Errorf("telemetry tmpfs file is not open")
	}

	headerBuffer := make([]byte, TmpfsHeaderSize)
	for retries := 0; retries < telemetryHeaderRetries; retries++ {
		select {
		case <-stopCh:
			return 0, 0, fmt.Errorf("stopped")
		default:
		}

		if _, readErr := file.ReadAt(headerBuffer, 0); readErr != nil {
			return 0, 0, fmt.Errorf("read telemetry header: %w", readErr)
		}
		initialSequence := binary.LittleEndian.Uint64(headerBuffer[tmpfsHeaderSeqStart:tmpfsHeaderWriteStart])
		if initialSequence%2 != 0 {
			time.Sleep(time.Millisecond)
			continue
		}

		writeOffset := binary.LittleEndian.Uint64(headerBuffer[tmpfsHeaderWriteStart:tmpfsHeaderReadStart])
		consumedOffset := binary.LittleEndian.Uint64(headerBuffer[tmpfsHeaderReadStart:TmpfsHeaderSize])

		if _, readErr := file.ReadAt(headerBuffer[tmpfsHeaderSeqStart:tmpfsHeaderWriteStart], 0); readErr != nil {
			return 0, 0, fmt.Errorf("reread telemetry sequence: %w", readErr)
		}
		confirmedSequence := binary.LittleEndian.Uint64(headerBuffer[tmpfsHeaderSeqStart:tmpfsHeaderWriteStart])
		if initialSequence == confirmedSequence {
			return writeOffset, consumedOffset, nil
		}
	}
	return 0, 0, fmt.Errorf("seqlock retry exhausted")
}

func shouldSendTelemetryAck(bytesSinceAck int, lastAckTime time.Time, byteThreshold int, timeThreshold time.Duration) bool {
	if bytesSinceAck <= 0 {
		return false
	}
	if byteThreshold > 0 && bytesSinceAck >= byteThreshold {
		return true
	}
	return timeThreshold > 0 && !lastAckTime.IsZero() && time.Since(lastAckTime) >= timeThreshold
}

func sleepOrStop(duration time.Duration, stopCh <-chan struct{}) bool {
	if duration <= 0 {
		duration = DefaultPollInterval
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-stopCh:
		return false
	}
}

func errOrJoin(primaryErr, secondaryErr error) error {
	if secondaryErr == nil {
		return primaryErr
	}
	return fmt.Errorf("%w; close telemetry tmpfs file: %v", primaryErr, secondaryErr)
}
