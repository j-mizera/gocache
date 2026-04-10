package rex

import "testing"

func TestInjectIntoHookCtx_ConnOnly(t *testing.T) {
	s := NewStore()
	_ = s.Set("auth.jwt", "tok1")
	_ = s.Set("tenant.id", "team-a")

	hookCtx := make(map[string]string)
	InjectIntoHookCtx(hookCtx, s, nil)

	if hookCtx["shared.rex.auth.jwt"] != "tok1" {
		t.Errorf("auth.jwt: got %q", hookCtx["shared.rex.auth.jwt"])
	}
	if hookCtx["shared.rex.tenant.id"] != "team-a" {
		t.Errorf("tenant.id: got %q", hookCtx["shared.rex.tenant.id"])
	}
}

func TestInjectIntoHookCtx_CmdOnly(t *testing.T) {
	cmdMeta := map[string]string{
		"traceparent": "00-abc",
	}

	hookCtx := make(map[string]string)
	InjectIntoHookCtx(hookCtx, nil, cmdMeta)

	if hookCtx["shared.rex.traceparent"] != "00-abc" {
		t.Errorf("traceparent: got %q", hookCtx["shared.rex.traceparent"])
	}
}

func TestInjectIntoHookCtx_CmdOverridesConn(t *testing.T) {
	s := NewStore()
	_ = s.Set("auth.jwt", "old-token")
	_ = s.Set("tenant.id", "team-a")

	cmdMeta := map[string]string{
		"auth.jwt": "new-token",
	}

	hookCtx := make(map[string]string)
	InjectIntoHookCtx(hookCtx, s, cmdMeta)

	if hookCtx["shared.rex.auth.jwt"] != "new-token" {
		t.Errorf("auth.jwt: got %q, want new-token", hookCtx["shared.rex.auth.jwt"])
	}
	if hookCtx["shared.rex.tenant.id"] != "team-a" {
		t.Errorf("tenant.id: got %q, want team-a", hookCtx["shared.rex.tenant.id"])
	}
}

func TestInjectIntoHookCtx_NilBoth(t *testing.T) {
	hookCtx := make(map[string]string)
	hookCtx["existing"] = "val"
	InjectIntoHookCtx(hookCtx, nil, nil)

	if len(hookCtx) != 1 || hookCtx["existing"] != "val" {
		t.Errorf("unexpected mutation: %v", hookCtx)
	}
}

func TestInjectIntoHookCtx_PreservesExisting(t *testing.T) {
	hookCtx := map[string]string{
		"_start_ns":     "12345",
		"shared.plugin": "val",
	}
	cmdMeta := map[string]string{"traceparent": "00-abc"}
	InjectIntoHookCtx(hookCtx, nil, cmdMeta)

	if hookCtx["_start_ns"] != "12345" {
		t.Error("_start_ns was overwritten")
	}
	if hookCtx["shared.plugin"] != "val" {
		t.Error("shared.plugin was overwritten")
	}
	if hookCtx["shared.rex.traceparent"] != "00-abc" {
		t.Error("traceparent not injected")
	}
}
