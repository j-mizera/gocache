//go:build otlp

package otlp

import (
	"context"
	"testing"

	"gocache/pkg/config"
)

// TestBootInit_DisabledNoEndpoint verifies the common case: user built
// with the tag but did not configure OTLP. BootInit must be a no-op and
// leave the plugin in a state where ConfigLoaded + ProcessShutdown are
// also safe no-ops (no nil-ptr panics).
func TestBootInit_DisabledNoEndpoint(t *testing.T) {
	p := &plugin{service: defaultService, timeout: defaultTimeout}
	// No env vars set → endpoint stays empty → plugin soft-disables.
	if err := p.BootInit(context.Background()); err != nil {
		t.Fatalf("BootInit with no endpoint should be nil, got %v", err)
	}
	if p.provider != nil {
		t.Errorf("provider should remain nil when endpoint is empty")
	}
	// Subsequent lifecycle calls must not panic.
	if err := p.ConfigLoaded(context.Background(), config.DefaultConfig()); err != nil {
		t.Errorf("ConfigLoaded unexpected error: %v", err)
	}
	if err := p.ProcessShutdown(context.Background()); err != nil {
		t.Errorf("ProcessShutdown unexpected error: %v", err)
	}
}

func TestBootInit_ExplicitDisabled(t *testing.T) {
	t.Setenv(envDisabled, "true")
	t.Setenv(envEndpoint, "http://localhost:4318") // would work, but disabled wins
	p := &plugin{service: defaultService, timeout: defaultTimeout}
	if err := p.BootInit(context.Background()); err != nil {
		t.Fatalf("BootInit with disabled=true should be nil, got %v", err)
	}
	if p.provider != nil {
		t.Errorf("provider should remain nil when disabled")
	}
}

func TestApplyEnv(t *testing.T) {
	t.Setenv(envEndpoint, "https://collector.example:4318")
	t.Setenv(envService, "test-service")
	t.Setenv(envTimeoutMs, "5000")

	p := &plugin{service: defaultService, timeout: defaultTimeout}
	p.applyEnv()

	if p.endpoint != "https://collector.example:4318" {
		t.Errorf("endpoint = %q", p.endpoint)
	}
	if p.service != "test-service" {
		t.Errorf("service = %q", p.service)
	}
	if p.timeout.Milliseconds() != 5000 {
		t.Errorf("timeout = %s, want 5s", p.timeout)
	}
	// https endpoint → insecure should stay false.
	if p.insecure {
		t.Errorf("https endpoint should default to secure")
	}
}

func TestApplyEnv_HTTPDefaultsInsecure(t *testing.T) {
	t.Setenv(envEndpoint, "http://localhost:4318")
	p := &plugin{service: defaultService, timeout: defaultTimeout}
	p.applyEnv()
	if !p.insecure {
		t.Errorf("http endpoint should default to insecure")
	}
}
