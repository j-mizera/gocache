package embedded

import (
	"context"
	"errors"
	"testing"

	"gocache/pkg/config"
)

type recordingPlugin struct {
	name         string
	bootErr      error
	bootPanic    any
	cfgErr       error
	shutdownErr  error
	booted       bool
	configured   bool
	shutDown     bool
	boots        int
	configs      int
	shutdowns    int
	lastBootCtx  context.Context
	lastShutdown context.Context
}

func (p *recordingPlugin) Name() string { return p.name }

func (p *recordingPlugin) BootInit(ctx context.Context) error {
	p.boots++
	p.booted = true
	p.lastBootCtx = ctx
	if p.bootPanic != nil {
		panic(p.bootPanic)
	}
	return p.bootErr
}

func (p *recordingPlugin) ConfigLoaded(_ context.Context, _ *config.Config) error {
	p.configs++
	p.configured = true
	return p.cfgErr
}

func (p *recordingPlugin) ProcessShutdown(ctx context.Context) error {
	p.shutdowns++
	p.shutDown = true
	p.lastShutdown = ctx
	return p.shutdownErr
}

func TestEmbedded_Register_BootAll_RunsInOrder(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	a := &recordingPlugin{name: "a"}
	b := &recordingPlugin{name: "b"}
	Register(a)
	Register(b)

	if Count() != 2 {
		t.Fatalf("Count = %d, want 2", Count())
	}
	if got := Names(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Names = %v, want [a b]", got)
	}

	BootAll(context.Background())

	if !a.booted || !b.booted {
		t.Fatal("BootAll did not invoke all plugins")
	}
	if a.boots != 1 || b.boots != 1 {
		t.Fatalf("expected single boot each, got a=%d b=%d", a.boots, b.boots)
	}
}

func TestEmbedded_BootAll_NonStrictErrorContinues(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	a := &recordingPlugin{name: "failing", bootErr: errors.New("boom")}
	b := &recordingPlugin{name: "healthy"}
	Register(a)
	Register(b)

	BootAll(context.Background())

	// a returned error; b should still have booted.
	if !b.booted {
		t.Fatal("subsequent plugin skipped after non-strict plugin error")
	}
}

func TestEmbedded_BootAll_NonStrictPanicRecovers(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	a := &recordingPlugin{name: "panicky", bootPanic: "kaboom"}
	b := &recordingPlugin{name: "healthy"}
	Register(a)
	Register(b)

	// Should not propagate the panic — embedded plugin failures are isolated.
	BootAll(context.Background())

	if !b.booted {
		t.Fatal("subsequent plugin skipped after panic — recover is broken")
	}
}

func TestEmbedded_ConfigLoadedAll_RunsAllWithCfg(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	a := &recordingPlugin{name: "a"}
	Register(a)

	cfg := config.DefaultConfig()
	ConfigLoadedAll(context.Background(), cfg)

	if !a.configured {
		t.Fatal("ConfigLoaded not invoked")
	}
}

func TestEmbedded_ShutdownAll_ReverseOrder(t *testing.T) {
	t.Cleanup(ResetForTesting)
	ResetForTesting()

	var order []string
	record := func(name string) *recordingPlugin {
		p := &recordingPlugin{name: name}
		// Stash the order hook on the shutdown callback by overriding via closure.
		return p
	}
	a := record("a")
	b := record("b")
	c := record("c")
	Register(a)
	Register(b)
	Register(c)

	// Use a custom shutdown that records invocation order via a shared slice.
	// Because ProcessShutdown is on the interface we can't inject cleanly;
	// instead assert on counters + the concrete order by reading shutdown
	// timestamps or a side channel. We use a shared slice via a wrapper.
	a.shutdownErr = nil
	b.shutdownErr = nil
	c.shutdownErr = nil

	// Replace plugins with wrappers that append to `order`.
	ResetForTesting()
	Register(&orderedPlugin{name: "a", order: &order})
	Register(&orderedPlugin{name: "b", order: &order})
	Register(&orderedPlugin{name: "c", order: &order})

	ShutdownAll(context.Background())

	want := []string{"c", "b", "a"} // reverse registration order
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("shutdown order = %v, want %v", order, want)
	}
}

type orderedPlugin struct {
	name  string
	order *[]string
}

func (p *orderedPlugin) Name() string                                        { return p.name }
func (p *orderedPlugin) BootInit(context.Context) error                      { return nil }
func (p *orderedPlugin) ConfigLoaded(context.Context, *config.Config) error  { return nil }
func (p *orderedPlugin) ProcessShutdown(context.Context) error {
	*p.order = append(*p.order, p.name)
	return nil
}
