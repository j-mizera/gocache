package manager

import (
	"testing"
)

func TestRegistry_AddGetRemove(t *testing.T) {
	r := NewRegistry()

	p := &PluginInstance{Name: "auth", BinPath: "/bin/auth"}
	p.SetState(StateLoaded)
	r.Add(p)

	got, ok := r.Get("auth")
	if !ok {
		t.Fatal("expected plugin to exist")
	}
	if got.BinPath != "/bin/auth" {
		t.Errorf("expected /bin/auth, got %s", got.BinPath)
	}

	r.Remove("auth")
	_, ok = r.Get("auth")
	if ok {
		t.Error("expected plugin to be removed")
	}
}

func TestRegistry_All(t *testing.T) {
	r := NewRegistry()
	r.Add(&PluginInstance{Name: "a"})
	r.Add(&PluginInstance{Name: "b"})
	r.Add(&PluginInstance{Name: "c"})

	all := r.All()
	if len(all) != 3 {
		t.Errorf("expected 3, got %d", len(all))
	}
}

func TestRegistry_SetState(t *testing.T) {
	r := NewRegistry()
	inst := &PluginInstance{Name: "x"}
	inst.SetState(StateLoaded)
	r.Add(inst)

	r.SetState("x", StateRunning)

	p, _ := r.Get("x")
	if got := p.State(); got != StateRunning {
		t.Errorf("expected Running, got %s", got)
	}
}

func TestRegistry_Len(t *testing.T) {
	r := NewRegistry()
	if r.Len() != 0 {
		t.Error("expected 0")
	}
	r.Add(&PluginInstance{Name: "p"})
	if r.Len() != 1 {
		t.Error("expected 1")
	}
}

func TestPluginState_String(t *testing.T) {
	tests := []struct {
		state PluginState
		want  string
	}{
		{StateLoaded, "loaded"},
		{StateStarting, "starting"},
		{StateConnected, "connected"},
		{StateRegistered, "registered"},
		{StateRunning, "running"},
		{StateUnhealthy, "unhealthy"},
		{StateRestarting, "restarting"},
		{StateShutdown, "shutdown"},
		{PluginState(99), "unknown(99)"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}
