package logcollector

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

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

func (m *mockEmitter) HasSubscribers() bool                           { return true }
func (m *mockEmitter) HasSubscribersFor(types ...apiEvents.Type) bool { return true }

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

func onlyRecord(t *testing.T, em *mockEmitter) apiEvents.Event {
	t.Helper()
	if em.count() != 1 {
		t.Fatalf("expected 1 event, got %d", em.count())
	}
	evt := em.get(0)
	batch := evt.Proto.GetRuntimeLogBatch()
	if batch == nil {
		t.Fatal("expected RuntimeLogBatchEventV1")
	}
	if len(batch.Records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(batch.Records))
	}
	return evt
}

func waitForCount(t *testing.T, em *mockEmitter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if em.count() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("expected %d events, got %d", want, em.count())
}

func TestCollector_BasicJSONLine(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader(`{"level":"info","source":"server","message":"hello world","time":"2026-01-01T00:00:00Z"}` + "\n")
	c.AddSource("server", r)
	c.Wait()

	record := onlyRecord(t, em).Proto.GetRuntimeLogBatch().Records[0]
	if record.Level != "info" {
		t.Errorf("expected level info, got %q", record.Level)
	}
	if record.Message != "hello world" {
		t.Errorf("expected 'hello world', got %q", record.Message)
	}
	if record.Source != "server" {
		t.Errorf("expected source server, got %q", record.Source)
	}
	if record.Timestamp == 0 {
		t.Error("expected timestamp to be set")
	}
}

func TestCollector_OperationID(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader(`{"level":"info","message":"cache hit","_operation_id":"cmd_42","source":"server"}` + "\n")
	c.AddSource("server", r)
	c.Wait()

	record := onlyRecord(t, em).Proto.GetRuntimeLogBatch().Records[0]
	if record.OperationId != "cmd_42" {
		t.Errorf("expected operation_id cmd_42, got %q", record.OperationId)
	}
}

func TestCollector_ContextPassthrough(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader(`{"level":"info","message":"test","_operation_id":"cmd_1","_ctx":{"shared.traceparent":"00-abc-def-01","_command":"SET","shared.username":"john"},"source":"server"}` + "\n")
	c.AddSource("server", r)
	c.Wait()

	record := onlyRecord(t, em).Proto.GetRuntimeLogBatch().Records[0]

	// _ctx fields should be merged into the record fields.
	if record.Fields["shared.traceparent"] != "00-abc-def-01" {
		t.Errorf("expected traceparent in fields, got %v", record.Fields)
	}
	if record.Fields["_command"] != "SET" {
		t.Errorf("expected _command in fields, got %v", record.Fields)
	}
	if record.Fields["shared.username"] != "john" {
		t.Errorf("expected username in fields, got %v", record.Fields)
	}
}

func TestCollector_ExtraFields(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader(`{"level":"warn","message":"slow","key":"user:123","elapsed_ms":500,"source":"server"}` + "\n")
	c.AddSource("server", r)
	c.Wait()

	record := onlyRecord(t, em).Proto.GetRuntimeLogBatch().Records[0]
	if record.Fields["key"] != "user:123" {
		t.Errorf("expected key field, got %v", record.Fields)
	}
	if record.Fields["elapsed_ms"] != "500" {
		t.Errorf("expected elapsed_ms=500, got %q", record.Fields["elapsed_ms"])
	}
}

func TestCollector_NonJSON(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	r := strings.NewReader("this is not json\n")
	c.AddSource("plugin-x", r)
	c.Wait()

	record := onlyRecord(t, em).Proto.GetRuntimeLogBatch().Records[0]
	if record.Message != "this is not json" {
		t.Errorf("expected raw message, got %q", record.Message)
	}
	if record.Source != "plugin-x" {
		t.Errorf("expected source plugin-x, got %q", record.Source)
	}
	if record.Fields["_raw"] != "true" {
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
	r2 := strings.NewReader(`{"level":"info","message":"from plugin","source":"prometheus"}` + "\n")

	c.AddSource("server", r1)
	c.AddSource("prometheus", r2)
	c.Wait()

	if em.count() != 1 {
		t.Fatalf("expected 1 batched event, got %d", em.count())
	}
	batch := em.get(0).Proto.GetRuntimeLogBatch()
	if batch == nil || len(batch.Records) != 2 {
		t.Fatalf("expected 2 batched records, got %#v", batch)
	}
}

func TestCollector_SourceFallback(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	// No "source" field in JSON — should use the source name from AddSource.
	r := strings.NewReader(`{"level":"info","message":"hello"}` + "\n")
	c.AddSource("my-plugin", r)
	c.Wait()

	record := onlyRecord(t, em).Proto.GetRuntimeLogBatch().Records[0]
	if record.Source != "my-plugin" {
		t.Errorf("expected fallback source my-plugin, got %q", record.Source)
	}
}

func TestCollector_DoesNotEmitBeforePeriodicFlush(t *testing.T) {
	em := &mockEmitter{}
	c := New(em, WithFlushInterval(time.Hour))

	c.parseLine("server", []byte(`{"level":"info","message":"buffered"}`))
	if em.count() != 0 {
		t.Fatalf("expected no immediate event before periodic flush, got %d", em.count())
	}

	c.Wait()
	if em.count() != 1 {
		t.Fatalf("expected Wait to flush remaining batch, got %d", em.count())
	}
}

func TestCollector_PeriodicFlush(t *testing.T) {
	em := &mockEmitter{}
	c := New(em, WithFlushInterval(5*time.Millisecond), WithMaxBatchSize(100))
	t.Cleanup(c.Wait)

	c.parseLine("server", []byte(`{"level":"info","message":"tick"}`))
	waitForCount(t, em, 1)
}

func TestCollector_MaxBatchSizeFlushesSafetyBatch(t *testing.T) {
	em := &mockEmitter{}
	c := New(em, WithFlushInterval(time.Hour), WithMaxBatchSize(2))

	c.parseLine("server", []byte(`{"level":"info","message":"first"}`))
	if em.count() != 0 {
		t.Fatalf("expected first record to remain buffered, got %d events", em.count())
	}
	c.parseLine("server", []byte(`{"level":"info","message":"second"}`))
	if em.count() != 1 {
		t.Fatalf("expected full batch to flush, got %d events", em.count())
	}
	batch := em.get(0).Proto.GetRuntimeLogBatch()
	if batch == nil || len(batch.Records) != 2 {
		t.Fatalf("expected 2 records in safety batch, got %#v", batch)
	}
	c.Wait()
}

func TestCollector_PipeSimulation(t *testing.T) {
	em := &mockEmitter{}
	c := New(em)

	// Simulate a pipe: writer writes lines, reader reads them.
	pr, pw := io.Pipe()

	c.AddSource("server", pr)

	go func() {
		_, _ = pw.Write([]byte(`{"level":"info","message":"line1","source":"server"}` + "\n"))
		_, _ = pw.Write([]byte(`{"level":"warn","message":"line2","source":"server"}` + "\n"))
		_ = pw.Close()
	}()

	c.Wait()

	if em.count() != 1 {
		t.Fatalf("expected 1 batched event from pipe, got %d", em.count())
	}
	batch := em.get(0).Proto.GetRuntimeLogBatch()
	if batch == nil || len(batch.Records) != 2 {
		t.Fatalf("expected 2 pipe records, got %#v", batch)
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
			_, _ = w.Write([]byte(`{"level":"info","message":"concurrent"}` + "\n"))
			_ = w.Close()
		}(pw)
	}

	wg.Wait()
	c.Wait()

	if em.count() != 1 {
		t.Fatalf("expected 1 batched event, got %d", em.count())
	}
	batch := em.get(0).Proto.GetRuntimeLogBatch()
	if batch == nil || len(batch.Records) != n {
		t.Fatalf("expected %d records, got %#v", n, batch)
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

	record := onlyRecord(t, em).Proto.GetRuntimeLogBatch().Records[0]
	if record.Fields["key_0"] != "value_0" {
		t.Error("expected _ctx fields to be merged")
	}
	if record.Fields["key_99"] != "value_99" {
		t.Error("expected all 100 _ctx fields")
	}
}
