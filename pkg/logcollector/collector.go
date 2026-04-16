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
	"fmt"
	"io"
	"sync"

	"gocache/api/events"
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
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)

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
	var raw map[string]interface{}
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
	operationID := stringField(raw, "_operation_id")

	// Extract _ctx (redacted operation context written by the logger).
	var ctxFields map[string]string
	if ctxRaw, ok := raw["_ctx"]; ok {
		if ctxMap, ok := ctxRaw.(map[string]interface{}); ok {
			ctxFields = make(map[string]string, len(ctxMap))
			for k, v := range ctxMap {
				if s, ok := v.(string); ok {
					ctxFields[k] = s
				}
			}
		}
	}

	// Build fields from remaining keys (exclude well-known).
	fields := make(map[string]string)
	fields["_source"] = source
	for k, v := range raw {
		switch k {
		case "level", "message", "time", "source", "_operation_id", "_ctx":
			continue
		default:
			switch val := v.(type) {
			case string:
				fields[k] = val
			case float64:
				// JSON numbers are float64.
				if val == float64(int64(val)) {
					fields[k] = json.Number(fmt.Sprintf("%d", int64(val))).String()
				} else {
					fields[k] = json.Number(fmt.Sprintf("%g", val)).String()
				}
			case bool:
				if val {
					fields[k] = "true"
				} else {
					fields[k] = "false"
				}
			default:
				if b, err := json.Marshal(val); err == nil {
					fields[k] = string(b)
				}
			}
		}
	}

	// Merge _ctx fields into the event fields. The _ctx contains the redacted
	// operation context (shared.traceparent, _command, etc.). These flow through
	// to the LogEntry event so subscribers can correlate.
	for k, v := range ctxFields {
		fields[k] = v
	}

	evt := events.NewLogEntry(level, message, "", fields)
	if operationID != "" {
		evt = evt.WithOperationID(operationID)
	}

	c.emitter.Emit(evt)
}

func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
