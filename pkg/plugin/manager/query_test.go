package manager

import (
	"net"
	"testing"
	"time"

	"gocache/commons/transport"
	"gocache/pkg/plugin/router"
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
	_, err := qr.Handle("nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for unknown topic")
	}
}

func TestQueryRegistry_RegisterAndHandle(t *testing.T) {
	qr := NewQueryRegistry()
	qr.Register("test", func(_ map[string]string) (map[string]string, error) {
		return map[string]string{"key": "value"}, nil
	})

	data, err := qr.Handle("test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["key"] != "value" {
		t.Errorf("expected 'value', got %q", data["key"])
	}
}

func TestQueryRegistry_Topics(t *testing.T) {
	qr := NewQueryRegistry()
	qr.Register("a", func(_ map[string]string) (map[string]string, error) { return nil, nil })
	qr.Register("b", func(_ map[string]string) (map[string]string, error) { return nil, nil })

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
	data, err := handler(nil)
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
	data, err := handler(nil)
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
	data, err := handler(nil)
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
	authInst := &PluginInstance{Name: "auth"}
	authInst.SetState(StateRunning)
	authInst.SetCritical(true)
	metricsInst := &PluginInstance{Name: "metrics"}
	metricsInst.SetState(StateRunning)
	metricsInst.SetCritical(false)
	reg.Add(authInst)
	reg.Add(metricsInst)

	handler := pluginsHandler(reg)
	data, err := handler(nil)
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
	RegisterBuiltinHandlers(qr, reg, func() []router.PluginConnStats { return nil }, sp)

	topics := qr.Topics()
	expected := map[string]bool{"health": false, "stats": false, "plugins": false, "plugin.ipc": false}
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
	RegisterBuiltinHandlers(qr, reg, func() []router.PluginConnStats { return nil }, nil)

	if _, err := qr.Handle("plugins", nil); err != nil {
		t.Errorf("plugins handler should be registered: %v", err)
	}
	if _, err := qr.Handle("plugin.ipc", nil); err != nil {
		t.Errorf("plugin.ipc handler should be registered: %v", err)
	}
	if _, err := qr.Handle("health", nil); err == nil {
		t.Error("health handler should NOT be registered without state provider")
	}
}

func TestManagerPluginIPCStatsIncludesEventOnlyConnectionsSorted(t *testing.T) {
	m := &Manager{}
	zetaServer, zetaClient := net.Pipe()
	alphaServer, alphaClient := net.Pipe()
	defer zetaClient.Close()
	defer alphaClient.Close()

	zeta := router.NewPluginConn("zeta", transport.NewConn(zetaServer))
	alpha := router.NewPluginConn("alpha", transport.NewConn(alphaServer))
	defer zeta.Close()
	defer alpha.Close()

	m.pluginConns.Store("zeta", zeta)
	m.pluginConns.Store("alpha", alpha)

	stats := m.pluginIPCStats()
	if len(stats) != 2 {
		t.Fatalf("len(stats)=%d, want 2", len(stats))
	}
	if stats[0].PluginName != "alpha" || stats[1].PluginName != "zeta" {
		t.Fatalf("stats order=%q,%q; want alpha,zeta", stats[0].PluginName, stats[1].PluginName)
	}
}

func TestPluginIPCHandler(t *testing.T) {
	handler := pluginIPCHandler(func() []router.PluginConnStats {
		return []router.PluginConnStats{
			{
				PluginName:                 "prometheus",
				QueueCapacity:              1024,
				QueueDepth:                 7,
				SendAttempts:               11,
				SendAccepted:               10,
				SendQueueFull:              1,
				BlockingSendAttempts:       3,
				BlockingSendLatencyTotalNs: 111,
				BlockingSendLatencyMaxNs:   22,
				FireAndForgetAttempts:      8,
				FireAndForgetAccepted:      7,
				FireAndForgetDrops:         1,
				EnqueueLatencyTotalNs:      123,
				EnqueueLatencyMaxNs:        45,
				WriteAttempts:              9,
				WriteErrors:                2,
				WriteBatches:               4,
				WriteBatchEnvelopes:        9,
				WriteBatchMaxSize:          3,
				WriteLatencyTotalNs:        456,
				WriteLatencyMaxNs:          78,
				QueueLagTotalNs:            789,
				QueueLagMaxNs:              90,
			},
		}
	})

	data, err := handler(nil)
	if err != nil {
		t.Fatalf("pluginIPCHandler error: %v", err)
	}
	checks := map[string]string{
		"prometheus.queue_capacity":                 "1024",
		"prometheus.queue_depth":                    "7",
		"prometheus.send_attempts":                  "11",
		"prometheus.send_accepted":                  "10",
		"prometheus.send_queue_full":                "1",
		"prometheus.blocking_send_attempts":         "3",
		"prometheus.blocking_send_latency_total_ns": "111",
		"prometheus.blocking_send_latency_max_ns":   "22",
		"prometheus.fire_and_forget_attempts":       "8",
		"prometheus.fire_and_forget_accepted":       "7",
		"prometheus.fire_and_forget_drops":          "1",
		"prometheus.enqueue_latency_total_ns":       "123",
		"prometheus.enqueue_latency_max_ns":         "45",
		"prometheus.write_attempts":                 "9",
		"prometheus.write_errors":                   "2",
		"prometheus.write_batches":                  "4",
		"prometheus.write_batch_envelopes":          "9",
		"prometheus.write_batch_max_size":           "3",
		"prometheus.write_latency_total_ns":         "456",
		"prometheus.write_latency_max_ns":           "78",
		"prometheus.queue_lag_total_ns":             "789",
		"prometheus.queue_lag_max_ns":               "90",
	}
	for key, want := range checks {
		if got := data[key]; got != want {
			t.Errorf("data[%q]=%q, want %q", key, got, want)
		}
	}
}
