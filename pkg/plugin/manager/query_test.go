package manager

import (
	"testing"
	"time"
)

type mockStateProvider struct {
	shuttingDown bool
	startTime    time.Time
	connections  int
	keys         int
	usedBytes    int64
	maxBytes     int64
}

func (m *mockStateProvider) IsShuttingDown() bool   { return m.shuttingDown }
func (m *mockStateProvider) StartTime() time.Time   { return m.startTime }
func (m *mockStateProvider) ActiveConnections() int { return m.connections }
func (m *mockStateProvider) CacheKeys() int         { return m.keys }
func (m *mockStateProvider) CacheUsedBytes() int64  { return m.usedBytes }
func (m *mockStateProvider) CacheMaxBytes() int64   { return m.maxBytes }

func TestQueryRegistry_UnknownTopic(t *testing.T) {
	qr := NewQueryRegistry()
	_, err := qr.Handle("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown topic")
	}
}

func TestQueryRegistry_RegisterAndHandle(t *testing.T) {
	qr := NewQueryRegistry()
	qr.Register("test", func() (map[string]string, error) {
		return map[string]string{"key": "value"}, nil
	})

	data, err := qr.Handle("test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["key"] != "value" {
		t.Errorf("expected 'value', got %q", data["key"])
	}
}

func TestQueryRegistry_Topics(t *testing.T) {
	qr := NewQueryRegistry()
	qr.Register("a", func() (map[string]string, error) { return nil, nil })
	qr.Register("b", func() (map[string]string, error) { return nil, nil })

	topics := qr.Topics()
	if len(topics) != 2 {
		t.Fatalf("expected 2 topics, got %d", len(topics))
	}
}

func TestHealthHandler_Ok(t *testing.T) {
	sp := &mockStateProvider{
		startTime:   time.Now().Add(-10 * time.Second),
		connections: 5,
	}
	handler := healthHandler(sp)
	data, err := handler()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["status"] != "ok" {
		t.Errorf("expected 'ok', got %q", data["status"])
	}
	if data["connections"] != "5" {
		t.Errorf("expected '5', got %q", data["connections"])
	}
}

func TestHealthHandler_ShuttingDown(t *testing.T) {
	sp := &mockStateProvider{
		shuttingDown: true,
		startTime:    time.Now(),
	}
	handler := healthHandler(sp)
	data, err := handler()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["status"] != "shutting_down" {
		t.Errorf("expected 'shutting_down', got %q", data["status"])
	}
}

func TestStatsHandler(t *testing.T) {
	sp := &mockStateProvider{
		keys:      1000,
		usedBytes: 5242880,
		maxBytes:  10485760,
	}
	handler := statsHandler(sp)
	data, err := handler()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["keys"] != "1000" {
		t.Errorf("expected '1000', got %q", data["keys"])
	}
	if data["memory_bytes"] != "5242880" {
		t.Errorf("expected '5242880', got %q", data["memory_bytes"])
	}
}

func TestPluginsHandler(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&PluginInstance{Name: "auth", State: StateRunning, Critical: true})
	reg.Add(&PluginInstance{Name: "metrics", State: StateRunning, Critical: false})

	handler := pluginsHandler(reg)
	data, err := handler()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["auth.state"] != "running" {
		t.Errorf("expected auth running, got %q", data["auth.state"])
	}
	if data["auth.critical"] != "true" {
		t.Errorf("expected auth critical=true, got %q", data["auth.critical"])
	}
	if data["metrics.critical"] != "false" {
		t.Errorf("expected metrics critical=false, got %q", data["metrics.critical"])
	}
}

func TestRegisterBuiltinHandlers(t *testing.T) {
	reg := NewRegistry()
	sp := &mockStateProvider{startTime: time.Now()}
	qr := NewQueryRegistry()
	RegisterBuiltinHandlers(qr, reg, sp)

	topics := qr.Topics()
	expected := map[string]bool{"health": false, "stats": false, "plugins": false}
	for _, topic := range topics {
		expected[topic] = true
	}
	for topic, found := range expected {
		if !found {
			t.Errorf("expected topic %q to be registered", topic)
		}
	}
}

func TestRegisterBuiltinHandlers_NilStateProvider(t *testing.T) {
	reg := NewRegistry()
	qr := NewQueryRegistry()
	RegisterBuiltinHandlers(qr, reg, nil)

	// Only "plugins" should be registered when state provider is nil.
	if _, err := qr.Handle("plugins"); err != nil {
		t.Errorf("plugins handler should be registered: %v", err)
	}
	if _, err := qr.Handle("health"); err == nil {
		t.Error("health handler should NOT be registered without state provider")
	}
}
