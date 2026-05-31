// Package logcollector reads JSON log lines from multiple sources (server pipe,
// plugin stdout pipes) and emits periodically flushed runtime log batches to the
// normal event stream.
//
// This is the single point that converts log output → runtime.logs events. The
// logger package writes JSON to stdout, plugins write JSON to their stdout, and
// this collector reads all pipes, parses JSON, buffers structured records, and
// flushes batches on a timer rather than at operation completion boundaries.
package logcollector

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"time"

	"gocache/api/command"
	apiEvents "gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
)

// Scanner buffer sizes — initial 64 KiB grows up to 256 KiB for long log
// lines (large redacted _ctx maps can exceed the default bufio.Scanner cap).
const (
	scannerInitBuf = 64 * 1024
	scannerMaxBuf  = 256 * 1024

	defaultFlushInterval = time.Second
	defaultMaxBatchSize  = 256
)

// Option configures a Collector.
type Option func(*Collector)

// WithFlushInterval sets how often buffered log records are flushed to the
// event stream. Non-positive values keep the default interval.
func WithFlushInterval(interval time.Duration) Option {
	return func(c *Collector) {
		if interval > 0 {
			c.flushInterval = interval
		}
	}
}

// WithMaxBatchSize sets the maximum number of records held before an immediate
// safety flush. Non-positive values keep the default size.
func WithMaxBatchSize(size int) Option {
	return func(c *Collector) {
		if size > 0 {
			c.maxBatchSize = size
		}
	}
}

// Collector reads JSON log lines from multiple sources and emits runtime log
// batch events.
type Collector struct {
	emitter       apiEvents.Emitter
	flushInterval time.Duration
	maxBatchSize  int

	wg sync.WaitGroup

	mu      sync.Mutex
	records []*gcpc.RuntimeLogRecordV1

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// New creates a log collector that emits batched records to the given emitter.
func New(emitter apiEvents.Emitter, opts ...Option) *Collector {
	c := &Collector{
		emitter:       emitter,
		flushInterval: defaultFlushInterval,
		maxBatchSize:  defaultMaxBatchSize,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	go c.runFlusher()
	return c
}

// AddSource registers an io.Reader as a log source and starts a goroutine
// to read from it. The source name is used when the log line does not contain
// a source field. Safe to call concurrently and after Start.
func (c *Collector) AddSource(name string, r io.Reader) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.readSource(name, r)
	}()
}

// Wait blocks until all source readers have finished (EOF or error), then
// stops the periodic flusher and emits any remaining buffered records.
func (c *Collector) Wait() {
	c.wg.Wait()
	c.stopOnce.Do(func() {
		close(c.stopCh)
		<-c.doneCh
	})
}

func (c *Collector) runFlusher() {
	ticker := time.NewTicker(c.flushInterval)
	defer func() {
		ticker.Stop()
		close(c.doneCh)
	}()

	for {
		select {
		case <-ticker.C:
			c.flush()
		case <-c.stopCh:
			c.flush()
			return
		}
	}
}

// readSource reads JSON lines from a single source and buffers runtime log
// records for periodic batch emission.
func (c *Collector) readSource(sourceName string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Increase buffer for long log lines (e.g. large _ctx).
	scanner.Buffer(make([]byte, 0, scannerInitBuf), scannerMaxBuf)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		c.parseLine(sourceName, line)
	}
	if err := scanner.Err(); err != nil {
		c.appendRecord(&gcpc.RuntimeLogRecordV1{
			Timestamp: uint64(time.Now().UnixNano()),
			Level:     "warn",
			Source:    sourceName,
			Message:   "log source scanner error",
			Fields: map[string]string{
				"error": err.Error(),
			},
		})
	}
}

// parseLine parses a single log line and buffers it for the next runtime.logs
// batch flush.
func (c *Collector) parseLine(sourceName string, line []byte) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		c.appendRecord(&gcpc.RuntimeLogRecordV1{
			Timestamp: uint64(time.Now().UnixNano()),
			Level:     "info",
			Source:    sourceName,
			Message:   string(line),
			Fields: map[string]string{
				"_raw": "true",
			},
		})
		return
	}

	// Extract well-known fields.
	level := stringField(raw, "level")
	message := stringField(raw, "message")
	source := stringField(raw, "source")
	if source == "" {
		source = sourceName
	}
	caller := stringField(raw, "caller")
	operationID := stringField(raw, command.OperationID)

	// Build the fields map directly: one allocation sized for the upper bound
	// (raw keys + potential _ctx keys). Unknown keys are written straight in;
	// _ctx entries are flattened in-place rather than merged.
	ctxMap, _ := raw[command.CtxField].(map[string]any)
	fields := make(map[string]string, len(raw)+len(ctxMap))
	for k, v := range raw {
		switch k {
		case "level", "message", "time", "timestamp", "source", "caller", command.OperationID, command.CtxField:
			continue
		default:
			if s, ok := formatJSONValue(v); ok {
				fields[k] = s
			}
		}
	}
	// _ctx contains the redacted operation context (shared.traceparent,
	// _command, etc.) — flatten it into fields so subscribers can correlate.
	for k, v := range ctxMap {
		if s, ok := v.(string); ok {
			fields[k] = s
		}
	}

	c.appendRecord(&gcpc.RuntimeLogRecordV1{
		Timestamp:   timestampNanos(raw),
		OperationId: operationID,
		Level:       level,
		Source:      source,
		Message:     message,
		Caller:      caller,
		Fields:      fields,
	})
}

func (c *Collector) appendRecord(record *gcpc.RuntimeLogRecordV1) {
	var batch []*gcpc.RuntimeLogRecordV1
	c.mu.Lock()
	c.records = append(c.records, record)
	if len(c.records) >= c.maxBatchSize {
		batch = c.records
		c.records = nil
	}
	c.mu.Unlock()

	c.emitBatch(batch)
}

func (c *Collector) flush() {
	c.mu.Lock()
	batch := c.records
	c.records = nil
	c.mu.Unlock()

	c.emitBatch(batch)
}

func (c *Collector) emitBatch(records []*gcpc.RuntimeLogRecordV1) {
	if len(records) == 0 {
		return
	}
	c.emitter.Emit(apiEvents.NewRuntimeLogBatch(records))
}

func timestampNanos(m map[string]any) uint64 {
	if s := stringField(m, "time"); s != "" {
		if ts, ok := parseTimestamp(s); ok {
			return ts
		}
	}
	if s := stringField(m, "timestamp"); s != "" {
		if ts, ok := parseTimestamp(s); ok {
			return ts
		}
	}
	return uint64(time.Now().UnixNano())
}

func parseTimestamp(value string) (uint64, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, value); err == nil {
			return uint64(ts.UnixNano()), true
		}
	}
	return 0, false
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// formatJSONValue renders a decoded JSON value as a string suitable for
// the log fields map. Returns ("", false) if the value cannot be serialised.
func formatJSONValue(v any) (string, bool) {
	switch val := v.(type) {
	case string:
		return val, true
	case float64:
		// JSON numbers decode as float64; detect integers and render cleanly.
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10), true
		}
		return strconv.FormatFloat(val, 'g', -1, 64), true
	case bool:
		return strconv.FormatBool(val), true
	case nil:
		return "", false
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}
