//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	apiEvents "gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	"gocache/sdk/pluginsdk"
	"gocache/testkit/integration"
)

func TestInstrumentationOTLP_TracesReachJaeger(t *testing.T) {
	jaeger := integration.StartJaeger(t)

	serviceName := "gocache-instrumentation-it"
	p := newPlugin(discardLogger())
	cfg := pluginsdk.NewRemoteConfig(map[string]string{
		keyEndpoint:  jaeger.OTLPEndpoint,
		keyService:   serviceName,
		keyInsecure:  "true",
		keyTimeoutMs: "3000",
	})
	p.OnConfigReload(cfg)
	if err := p.OnHealthCheck(context.Background()); err != nil {
		t.Fatalf("configure instrumentation plugin: %v", err)
	}

	ctx := context.Background()
	start := apiEvents.NewOperationStarted("cmd_1", "command", "", map[string]string{"_command": "PING"}).Proto
	p.HandleEvent(ctx, start)
	p.HandleEvent(ctx, apiEvents.NewRuntimeLogBatch([]*gcpc.RuntimeLogRecordV1{{
		Timestamp:   uint64(time.Now().UnixNano()),
		OperationId: "cmd_1",
		Level:       "info",
		Source:      "server",
		Message:     "ping completed",
		Fields:      map[string]string{"_command": "PING"},
	}}).Proto)
	p.HandleEvent(ctx, apiEvents.NewOperationCompleted("cmd_1", "command", 1_000_000, "completed", "", map[string]string{"_command": "PING"}).Proto)

	if err := p.OnShutdown(ctx); err != nil {
		t.Fatalf("shutdown instrumentation plugin: %v", err)
	}

	traces := integration.QueryTraces(t, jaeger.QueryEndpoint, serviceName, 10*time.Second)
	spans := integration.SpansByName(traces, "gocache.command.PING")
	if len(spans) == 0 {
		t.Fatal("expected gocache.command.PING span")
	}
	if got := spans[0].Tags[componentKey]; got != componentValue {
		t.Errorf("span component = %q, want %q", got, componentValue)
	}
	if got := spans[0].Tags["gocache.operation.id"]; got != "cmd_1" {
		t.Errorf("span operation id = %q, want cmd_1", got)
	}
}
