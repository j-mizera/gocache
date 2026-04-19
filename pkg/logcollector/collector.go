// Package logcollector reads JSON log lines from multiple sources (server pipe,
// plugin stdout pipes) and emits LogEntry events to the event bus.
//
// This is the single point that converts log output → events. The logger package
// writes JSON to stdout, plugins write JSON to their stdout, and this collector
// reads all pipes, parses JSON, and emits structured LogEntry events.
package logcollector

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"sync"

	"gocache/api/command"
	"gocache/api/events"
)

// Scanner buffer sizes — initial 64 KiB grows up to 256 KiB for long log
// lines (large redacted _ctx maps can exceed the default bufio.Scanner cap).
const (
	scannerInitBuf = 64 * 1024
	scannerMaxBuf  = 256 * 1024
)

// Collector reads JSON log lines from multiple sources and emits LogEntry events.
type Collector struct {
	emitter events.Emitter
	wg      sync.WaitGroup
}

// New creates a log collector that emits events to the given emitter.
func New(emitter events.Emitter) *Collector {
	return &Collector{
		emitter: emitter,
	}
}

// AddSource registers an io.Reader as a log source and starts a goroutine
// to read from it. The source name is used for logging if parse errors occur.
// Safe to call concurrently and after Start.
func (c *Collector) AddSource(name string, r io.Reader) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.readSource(name, r)
	}()
}

// Wait blocks until all source readers have finished (EOF or error).
func (c *Collector) Wait() {
	c.wg.Wait()
}

// readSource reads JSON lines from a single source and emits LogEntry events.
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
	// Scanner error is expected on pipe close — not logged.
}

// parseLine parses a single JSON log line and emits a LogEntry event.
func (c *Collector) parseLine(sourceName string, line []byte) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		// Not JSON — could be a plain text line from a plugin.
		// Emit as a raw log entry with no structured fields.
		c.emitter.Emit(events.NewLogEntry("info", string(line), "", map[string]string{
			"_source": sourceName,
			"_raw":    "true",
		}))
		return
	}

	// Extract well-known fields.
	level := stringField(raw, "level")
	message := stringField(raw, "message")
	source := stringField(raw, "source")
	if source == "" {
		source = sourceName
	}
	operationID := stringField(raw, command.OperationID)

	// Build the fields map directly: one allocation sized for the upper bound
	// (raw keys + potential _ctx keys + "_source"). Unknown keys are written
	// straight in; _ctx entries are flattened in-place rather than merged.
	ctxMap, _ := raw[command.CtxField].(map[string]any)
	fields := make(map[string]string, len(raw)+len(ctxMap)+1)
	fields["_source"] = source
	for k, v := range raw {
		switch k {
		case "level", "message", "time", "source", command.OperationID, command.CtxField:
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

	evt := events.NewLogEntry(level, message, "", fields)
	if operationID != "" {
		evt = evt.WithOperationID(operationID)
	}

	c.emitter.Emit(evt)
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
