package logcollector

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	apiEvents "gocache/api/events"
)

type mockEmitter struct {
	mu     sync.Mutex
	events []apiEvents.Event
}

func (m *mockEmitter) Emit(evt apiEvents.Event) {
	m.mu.Lock()
	m.events = append(m.events, evt)
	m.mu.Unlock()
}

func (m *mockEmitter) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

func (m *mockEmitter) get(i int) apiEvents.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events[i]
}

func TestCollector_BasicJSONLine(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader(`{"level":"info","source":"server","message":"hello world","time":"2026-01-01T00:00:00Z"}` + "\n")
	c.AddSource("server", r)
	c.Wait()

	if em.count() != 1 {
		t.Fatalf("expected 1 event, got %d", em.count())
	}

	evt := em.get(0)
	data := evt.Proto.GetLogEntry()
	if data == nil {
		t.Fatal("expected LogEntryEventV1")
	}
	if data.Level != "info" {
		t.Errorf("expected level info, got %q", data.Level)
	}
	if data.Message != "hello world" {
		t.Errorf("expected 'hello world', got %q", data.Message)
	}
	if data.Fields["_source"] != "server" {
		t.Errorf("expected source server, got %q", data.Fields["_source"])
	}
}

func TestCollector_OperationID(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader(`{"level":"info","message":"cache hit","_operation_id":"cmd_42","source":"server"}` + "\n")
	c.AddSource("server", r)
	c.Wait()

	evt := em.get(0)
	if evt.Proto.OperationId != "cmd_42" {
		t.Errorf("expected operation_id cmd_42, got %q", evt.Proto.OperationId)
	}
}

func TestCollector_ContextPassthrough(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader(`{"level":"info","message":"test","_operation_id":"cmd_1","_ctx":{"shared.traceparent":"00-abc-def-01","_command":"SET","shared.username":"john"},"source":"server"}` + "\n")
	c.AddSource("server", r)
	c.Wait()

	evt := em.get(0)
	data := evt.Proto.GetLogEntry()

	// _ctx fields should be merged into the event fields.
	if data.Fields["shared.traceparent"] != "00-abc-def-01" {
		t.Errorf("expected traceparent in fields, got %v", data.Fields)
	}
	if data.Fields["_command"] != "SET" {
		t.Errorf("expected _command in fields, got %v", data.Fields)
	}
	if data.Fields["shared.username"] != "john" {
		t.Errorf("expected username in fields, got %v", data.Fields)
	}
}

func TestCollector_ExtraFields(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader(`{"level":"warn","message":"slow","key":"user:123","elapsed_ms":500,"source":"server"}` + "\n")
	c.AddSource("server", r)
	c.Wait()

	data := em.get(0).Proto.GetLogEntry()
	if data.Fields["key"] != "user:123" {
		t.Errorf("expected key field, got %v", data.Fields)
	}
	if data.Fields["elapsed_ms"] != "500" {
		t.Errorf("expected elapsed_ms=500, got %q", data.Fields["elapsed_ms"])
	}
}

func TestCollector_NonJSON(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader("this is not json\n")
	c.AddSource("plugin-x", r)
	c.Wait()

	if em.count() != 1 {
		t.Fatalf("expected 1 event for non-JSON, got %d", em.count())
	}
	data := em.get(0).Proto.GetLogEntry()
	if data.Message != "this is not json" {
		t.Errorf("expected raw message, got %q", data.Message)
	}
	if data.Fields["_source"] != "plugin-x" {
		t.Errorf("expected source plugin-x, got %q", data.Fields["_source"])
	}
	if data.Fields["_raw"] != "true" {
		t.Error("expected _raw=true for non-JSON")
	}
}

func TestCollector_EmptyLines(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader("\n\n" + `{"level":"info","message":"hi"}` + "\n\n")
	c.AddSource("server", r)
	c.Wait()

	if em.count() != 1 {
		t.Fatalf("expected 1 event (empty lines skipped), got %d", em.count())
	}
}

func TestCollector_MultipleSources(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r1 := strings.NewReader(`{"level":"info","message":"from server","source":"server"}` + "\n")
	r2 := strings.NewReader(`{"level":"info","message":"from plugin","source":"gobservability"}` + "\n")

	c.AddSource("server", r1)
	c.AddSource("gobservability", r2)
	c.Wait()

	if em.count() != 2 {
		t.Fatalf("expected 2 events, got %d", em.count())
	}
}

func TestCollector_SourceFallback(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	// No "source" field in JSON — should use the source name from AddSource.
	r := strings.NewReader(`{"level":"info","message":"hello"}` + "\n")
	c.AddSource("my-plugin", r)
	c.Wait()

	data := em.get(0).Proto.GetLogEntry()
	if data.Fields["_source"] != "my-plugin" {
		t.Errorf("expected fallback source my-plugin, got %q", data.Fields["_source"])
	}
}

func TestCollector_PipeSimulation(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	// Simulate a pipe: writer writes lines, reader reads them.
	pr, pw := io.Pipe()

	c.AddSource("server", pr)

	go func() {
		pw.Write([]byte(`{"level":"info","message":"line1","source":"server"}` + "\n"))
		pw.Write([]byte(`{"level":"warn","message":"line2","source":"server"}` + "\n"))
		pw.Close()
	}()

	c.Wait()

	if em.count() != 2 {
		t.Fatalf("expected 2 events from pipe, got %d", em.count())
	}
}

func TestCollector_ConcurrentWrites(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	// Multiple sources writing concurrently.
	const n = 10
	var writers []*io.PipeWriter
	for i := 0; i < n; i++ {
		pr, pw := io.Pipe()
		writers = append(writers, pw)
		c.AddSource("source", pr)
	}

	var wg sync.WaitGroup
	for _, pw := range writers {
		wg.Add(1)
		go func(w *io.PipeWriter) {
			defer wg.Done()
			w.Write([]byte(`{"level":"info","message":"concurrent"}` + "\n"))
			w.Close()
		}(pw)
	}

	wg.Wait()
	c.Wait()

	if em.count() != n {
		t.Errorf("expected %d events, got %d", n, em.count())
	}
}

func TestCollector_LargeContext(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	// Build a large _ctx.
	var buf bytes.Buffer
	buf.WriteString(`{"level":"info","message":"big ctx","_operation_id":"cmd_1","_ctx":{`)
	for i := 0; i < 100; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`"key_`)
		buf.WriteString(fmt.Sprintf("%d", i))
		buf.WriteString(`":"value_`)
		buf.WriteString(fmt.Sprintf("%d", i))
		buf.WriteByte('"')
	}
	buf.WriteString("}}\n")

	c.AddSource("server", &buf)
	c.Wait()

	if em.count() != 1 {
		t.Fatalf("expected 1 event, got %d", em.count())
	}
	data := em.get(0).Proto.GetLogEntry()
	if data.Fields["key_0"] != "value_0" {
		t.Error("expected _ctx fields to be merged")
	}
	if data.Fields["key_99"] != "value_99" {
		t.Error("expected all 100 _ctx fields")
	}
}
