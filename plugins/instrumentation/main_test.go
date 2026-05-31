package main

import (
	"context"
	"io"
	"testing"

	otellog "go.opentelemetry.io/otel/log"

	apiEvents "gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	apilogger "gocache/commons/logger"
	"gocache/sdk/pluginsdk"
)

func discardLogger() *apilogger.Logger {
	return apilogger.New(io.Discard, pluginName, "debug")
}

func TestPluginRequestsOnlyEventScope(t *testing.T) {
	p := newPlugin(nil)
	scopes := p.Scopes()
	if len(scopes) != 1 || scopes[0] != "events" {
		t.Fatalf("Scopes() = %v, want [events]", scopes)
	}
}

func TestPluginSubscribesToOperationAndRuntimeLogEvents(t *testing.T) {
	p := newPlugin(nil)
	got := map[string]bool{}
	for _, eventType := range p.EventTypes() {
		got[eventType] = true
	}
	for _, want := range []apiEvents.Type{
		apiEvents.OperationStarted,
		apiEvents.OperationCompleted,
		apiEvents.RuntimeLogBatch,
		apiEvents.ReplayGap,
	} {
		if !got[string(want)] {
			t.Fatalf("EventTypes() missing %q: %v", want, p.EventTypes())
		}
	}
}

func TestConfigNoEndpointSoftDisables(t *testing.T) {
	p := newPlugin(discardLogger())
	cfg := pluginsdk.NewRemoteConfig(nil)

	p.OnConfigReload(cfg)

	if err := p.OnHealthCheck(context.Background()); err != nil {
		t.Fatalf("OnHealthCheck() error = %v", err)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.traceProv != nil || p.logProv != nil {
		t.Fatal("providers should stay nil without endpoint")
	}
}

func TestLogRecordRedactsSecrets(t *testing.T) {
	record := logRecord(&gcpc.RuntimeLogRecordV1{
		Timestamp:   1,
		OperationId: "cmd_1",
		Level:       "warn",
		Source:      "server",
		Message:     "hello",
		Fields: map[string]string{
			"shared.visible": "yes",
			"shared.secret.token": "no",
			"secret.token":   "no",
		},
	})

	attrs := map[string]string{}
	record.WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value.AsString()
		return true
	})
	if attrs["shared.visible"] != "yes" {
		t.Fatalf("expected visible field, got %v", attrs)
	}
	if _, ok := attrs["shared.secret.token"]; ok {
		t.Fatalf("secret field was exported: %v", attrs)
	}
	if _, ok := attrs["secret.token"]; ok {
		t.Fatalf("secret token was exported: %v", attrs)
	}
}

func TestHandleEventNoProviderIsNoop(t *testing.T) {
	p := newPlugin(discardLogger())
	p.HandleEvent(context.Background(), apiEvents.NewOperationCompleted("cmd_1", "command", 10, "completed", "", nil).Proto)
	p.HandleEvent(context.Background(), apiEvents.NewRuntimeLogBatch([]*gcpc.RuntimeLogRecordV1{{Message: "hello"}}).Proto)
}
