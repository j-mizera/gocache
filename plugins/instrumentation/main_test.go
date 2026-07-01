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

func TestPluginRequestsEventAndTelemetryScopes(t *testing.T) {
	p := newPlugin(nil)
	scopes := p.Scopes()
	if len(scopes) != 2 || scopes[0] != "events" || scopes[1] != "telemetry" {
		t.Fatalf("Scopes() = %v, want [events telemetry]", scopes)
	}
}

func TestPluginSubscribesToNonTelemetryEvents(t *testing.T) {
	p := newPlugin(nil)
	eventTypes := map[string]bool{}
	for _, eventType := range p.EventTypes() {
		eventTypes[eventType] = true
	}
	for _, want := range []apiEvents.Type{
		apiEvents.RuntimeLogBatch,
		apiEvents.ReplayGap,
	} {
		if !eventTypes[string(want)] {
			t.Fatalf("EventTypes() missing %q: %v", want, p.EventTypes())
		}
	}
	for _, telemetryEvent := range []apiEvents.Type{
		apiEvents.OperationStarted,
		apiEvents.OperationCompleted,
	} {
		if eventTypes[string(telemetryEvent)] {
			t.Fatalf("EventTypes() should not include telemetry event %q: %v", telemetryEvent, p.EventTypes())
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
			"shared.visible":      "yes",
			"shared.secret.token": "no",
			"secret.token":        "no",
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

func TestReconstructedTelemetryContextExportsSourceFilteredPublicKeys(t *testing.T) {
	reconstructed := pluginsdk.NewContextReconstructor().ProcessOperation(&gcpc.TelemetryOperation{
		OperationId: "op-ctx",
		InitialContext: []*gcpc.Tag{
			{Key: []byte("shared.rex.data"), Value: []byte("rex-visible")},
			{Key: []byte("traceparent"), Value: []byte("00-00000000000000000000000000000001-0000000000000001-01")},
			{Key: []byte("tenant"), Value: []byte("acme")},
			{Key: []byte("shared.secret.visible"), Value: []byte("public-by-source-policy")},
		},
		TelemetryItems: []*gcpc.TelemetryItem{{Kind: gcpc.TelemetryItemKind_TELEMETRY_ITEM_OPERATION_FINISH}},
	})
	if reconstructed == nil {
		t.Fatal("expected completed reconstruction")
	}
	for _, privateKey := range []string{"_password", "_secret"} {
		if _, ok := reconstructed.Context[privateKey]; ok {
			t.Fatalf("private key %q reached reconstructed context: %v", privateKey, reconstructed.Context)
		}
	}

	spanAttributes := map[string]string{}
	for _, spanAttribute := range reconstructedOperationAttributes(reconstructed) {
		spanAttributes[string(spanAttribute.Key)] = spanAttribute.Value.AsString()
	}
	for key, want := range map[string]string{
		"gocache.context.shared.rex.data":       "rex-visible",
		"gocache.context.traceparent":           "00-00000000000000000000000000000001-0000000000000001-01",
		"gocache.context.tenant":                "acme",
		"gocache.context.shared.secret.visible": "public-by-source-policy",
	} {
		if spanAttributes[key] != want {
			t.Fatalf("span attribute %q = %q, want %q; all=%v", key, spanAttributes[key], want, spanAttributes)
		}
	}
	for _, privateKey := range []string{"gocache.context._password", "gocache.context._secret"} {
		if _, ok := spanAttributes[privateKey]; ok {
			t.Fatalf("private context attribute %q was exported: %v", privateKey, spanAttributes)
		}
	}
}

func TestReconstructedCommandAttributesDoesNotExportRawArgs(t *testing.T) {
	commandAttributes := reconstructedCommandAttributes(pluginsdk.ReconstructedCommand{
		Name: "AUTH",
		Args: []string{"default", "super-secret-token"},
	})

	spanStringAttributes := map[string]string{}
	spanIntAttributes := map[string]int64{}
	for _, commandAttribute := range commandAttributes {
		switch string(commandAttribute.Key) {
		case "gocache.command.args":
			t.Fatalf("raw command args were exported: %v", commandAttributes)
		case "gocache.command.name", "gocache.command.status":
			spanStringAttributes[string(commandAttribute.Key)] = commandAttribute.Value.AsString()
		case "gocache.command.arg_count":
			spanIntAttributes[string(commandAttribute.Key)] = commandAttribute.Value.AsInt64()
		}
	}

	if spanStringAttributes["gocache.command.name"] != "AUTH" {
		t.Fatalf("expected command name AUTH, got %v", spanStringAttributes)
	}
	if spanStringAttributes["gocache.command.status"] != "ok" {
		t.Fatalf("expected command status ok, got %v", spanStringAttributes)
	}
	if spanIntAttributes["gocache.command.arg_count"] != 2 {
		t.Fatalf("expected command arg count 2, got %v", spanIntAttributes)
	}
	for _, spanAttribute := range spanStringAttributes {
		if spanAttribute == "default" || spanAttribute == "super-secret-token" {
			t.Fatalf("raw AUTH argument was exported in span attributes: %v", spanStringAttributes)
		}
	}
}

func TestReconstructedCommandAttributesDoesNotExportRawResult(t *testing.T) {
	commandAttributes := reconstructedCommandAttributes(pluginsdk.ReconstructedCommand{
		Name:   "GET",
		Args:   []string{"user:1"},
		Result: "cached-user-data",
	})

	spanStringAttributes := map[string]string{}
	spanIntAttributes := map[string]int64{}
	for _, commandAttribute := range commandAttributes {
		switch string(commandAttribute.Key) {
		case "gocache.command.result":
			t.Fatalf("raw command result was exported: %v", commandAttributes)
		case "gocache.command.name", "gocache.command.status":
			spanStringAttributes[string(commandAttribute.Key)] = commandAttribute.Value.AsString()
		case "gocache.command.arg_count":
			spanIntAttributes[string(commandAttribute.Key)] = commandAttribute.Value.AsInt64()
		}
	}

	if spanStringAttributes["gocache.command.name"] != "GET" {
		t.Fatalf("expected command name GET, got %v", spanStringAttributes)
	}
	if spanStringAttributes["gocache.command.status"] != "ok" {
		t.Fatalf("expected command status ok, got %v", spanStringAttributes)
	}
	if spanIntAttributes["gocache.command.arg_count"] != 1 {
		t.Fatalf("expected command arg count 1, got %v", spanIntAttributes)
	}
	for _, spanAttribute := range spanStringAttributes {
		if spanAttribute == "cached-user-data" {
			t.Fatalf("raw command result was exported in span attributes: %v", spanStringAttributes)
		}
	}
}

func TestHandleEventNoProviderIsNoop(t *testing.T) {
	p := newPlugin(discardLogger())
	p.HandleEvent(context.Background(), apiEvents.NewOperationCompleted("cmd_1", "command", 10, "completed", "", nil).Proto)
	p.HandleEvent(context.Background(), apiEvents.NewRuntimeLogBatch([]*gcpc.RuntimeLogRecordV1{{Message: "hello"}}).Proto)
}
