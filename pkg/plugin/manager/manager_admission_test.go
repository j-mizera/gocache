package manager

import (
	"net"
	"testing"
	"time"

	apiEvents "gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"
	"gocache/commons/transport"
	serverEvents "gocache/pkg/events"
	"gocache/pkg/plugin/router"
)

func TestAdmissionControlDropsWhenNoHeadroom(t *testing.T) {
	eventBus := serverEvents.NewBusWithCapacity(0)
	m := &Manager{eventBus: eventBus}
	eventBus.Subscribe("plugin:test-plugin", []apiEvents.Type{apiEvents.CommandCompleted}, func(apiEvents.Event) {})

	pc := newZeroHeadroomPluginConn(t, "test-plugin")
	evt := apiEvents.NewCommandCompleted("SET", []string{"key", "value"}, 1, "OK", "", nil).Proto
	before := pc.Stats()

	if m.admitEventForPlugin("test-plugin", evt, pc) {
		t.Fatal("admitEventForPlugin admitted event with no IPC headroom")
	}

	after := pc.Stats()
	if got := m.stats.eventInterestChecks.Load(); got != 1 {
		t.Fatalf("eventInterestChecks=%d, want 1", got)
	}
	if got := m.stats.eventInterestHits.Load(); got != 1 {
		t.Fatalf("eventInterestHits=%d, want 1", got)
	}
	if got := m.stats.eventCreditDrops.Load(); got != 1 {
		t.Fatalf("eventCreditDrops=%d, want 1", got)
	}
	if after.FireAndForgetAttempts != before.FireAndForgetAttempts {
		t.Fatalf("FireAndForgetAttempts changed from %d to %d; admission drop should not enqueue", before.FireAndForgetAttempts, after.FireAndForgetAttempts)
	}
}

func newZeroHeadroomPluginConn(t *testing.T, pluginName string) *router.PluginConn {
	t.Helper()
	server, client := net.Pipe()
	pc := router.NewPluginConn(pluginName, transport.NewConn(server))
	t.Cleanup(func() {
		pc.Close()
		_ = client.Close()
	})

	builder := func() *gcpcv1.EnvelopeV1 { return gcpcv1.NewHealthCheck() }
	pc.SendFireAndForgetLazy(builder)
	capacity := pc.Stats().QueueCapacity
	deadline := time.Now().Add(time.Second)
	for pc.Headroom() != capacity && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := pc.Headroom(); got != capacity {
		t.Fatalf("initial event was not drained to the writer: headroom=%d, want %d", got, capacity)
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for attempts := 0; pc.Headroom() > 0 && attempts < capacity*2; attempts++ {
			pc.SendFireAndForgetLazy(builder)
		}
		if pc.Headroom() == 0 {
			time.Sleep(5 * time.Millisecond)
			if pc.Headroom() == 0 {
				return pc
			}
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatalf("headroom=%d, want 0", pc.Headroom())
	return nil
}
