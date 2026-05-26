//go:build otlp && integration

package otlp

import (
	"context"
	"testing"
	"time"

	"gocache/api/config"
	"gocache/commons/plugincfg"
	"gocache/testkit/integration"
)

func TestOTLP_TracesReachJaeger(t *testing.T) {
	jaeger := integration.StartJaeger(t)

	serviceName := "gocache-otlp-it"
	t.Setenv(envEndpoint, jaeger.OTLPEndpoint)
	t.Setenv(envService, serviceName)
	t.Setenv(envInsecure, "true")

	p := &plugin{service: defaultService, timeout: defaultTimeout}

	ctx := context.Background()

	if err := p.BootInit(ctx); err != nil {
		t.Fatalf("BootInit: %v", err)
	}
	if p.provider == nil {
		t.Fatal("provider should be initialized after BootInit")
	}

	if err := p.ConfigLoaded(ctx, config.DefaultConfig(), plugincfg.NewMapConfig()); err != nil {
		t.Fatalf("ConfigLoaded: %v", err)
	}

	if err := p.ProcessShutdown(ctx); err != nil {
		t.Fatalf("ProcessShutdown: %v", err)
	}

	traces := integration.QueryTraces(t, jaeger.QueryEndpoint, serviceName, 10*time.Second)

	processSpans := integration.SpansByName(traces, spanProcess)
	if len(processSpans) == 0 {
		t.Error("expected gocache.process span")
	}

	configSpans := integration.SpansByName(traces, spanConfigLoaded)
	if len(configSpans) == 0 {
		t.Error("expected gocache.config_loaded span")
	}

	shutdownSpans := integration.SpansByName(traces, spanShutdown)
	if len(shutdownSpans) == 0 {
		t.Error("expected gocache.shutdown span")
	}

	// Verify the component attribute is set on process span.
	if len(processSpans) > 0 {
		if got := processSpans[0].Tags[attrComponent]; got != componentValue {
			t.Errorf("process span component = %q, want %q", got, componentValue)
		}
	}
}
