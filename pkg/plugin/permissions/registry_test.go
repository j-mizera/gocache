package permissions

import "testing"

func TestRegistryBasic(t *testing.T) {
	r := NewRegistry()

	// Empty registry.
	if got := r.GetScopes("unknown"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if r.HasScope("unknown", ScopeRead) {
		t.Error("expected false for unknown plugin")
	}

	// Register and retrieve.
	r.Register("auth", []Scope{ScopeHookPre, ScopeRead})
	scopes := r.GetScopes("auth")
	if len(scopes) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(scopes))
	}
	if !r.HasScope("auth", ScopeRead) {
		t.Error("expected auth to have read scope")
	}
	if !r.HasScope("auth", ScopeHookPre) {
		t.Error("expected auth to have hook:pre scope")
	}
	if r.HasScope("auth", ScopeWrite) {
		t.Error("expected auth to not have write scope")
	}
}

func TestRegistryHierarchy(t *testing.T) {
	r := NewRegistry()
	r.Register("kafka", []Scope{ScopeWrite})

	if !r.HasScope("kafka", ScopeRead) {
		t.Error("write should imply read")
	}
	if !r.HasScope("kafka", ScopeWrite) {
		t.Error("should have write directly")
	}
	if r.HasScope("kafka", ScopeAdmin) {
		t.Error("write should not imply admin")
	}
}

func TestRegistryAdminImpliesAll(t *testing.T) {
	r := NewRegistry()
	r.Register("cluster", []Scope{ScopeAdmin})

	if !r.HasScope("cluster", ScopeRead) {
		t.Error("admin should imply read")
	}
	if !r.HasScope("cluster", ScopeWrite) {
		t.Error("admin should imply write")
	}
	if !r.HasScope("cluster", ScopeAdmin) {
		t.Error("admin should have admin")
	}
	if r.HasScope("cluster", ScopeHookPre) {
		t.Error("admin should not imply hook:pre")
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := NewRegistry()
	r.Register("metrics", []Scope{ScopeRead, ScopeHookPost})

	if !r.HasScope("metrics", ScopeRead) {
		t.Fatal("expected read scope")
	}

	r.Unregister("metrics")

	if r.HasScope("metrics", ScopeRead) {
		t.Error("expected no scopes after unregister")
	}
	if got := r.GetScopes("metrics"); got != nil {
		t.Errorf("expected nil after unregister, got %v", got)
	}
}

func TestRegistryKeyScopes(t *testing.T) {
	r := NewRegistry()
	r.Register("kafka", []Scope{ScopeWrite, Scope("keys:kafka:*"), Scope("keys:events:*")})

	ks := r.KeyScopes("kafka")
	if len(ks) != 2 {
		t.Errorf("expected 2 key scopes, got %d", len(ks))
	}

	// Plugin without key scopes.
	r.Register("metrics", []Scope{ScopeRead})
	ks2 := r.KeyScopes("metrics")
	if len(ks2) != 0 {
		t.Errorf("expected 0 key scopes, got %d", len(ks2))
	}
}

func TestRegistryOverwrite(t *testing.T) {
	r := NewRegistry()
	r.Register("auth", []Scope{ScopeRead})
	r.Register("auth", []Scope{ScopeAdmin, ScopeHookPre})

	scopes := r.GetScopes("auth")
	if len(scopes) != 2 {
		t.Errorf("expected 2 scopes after overwrite, got %d", len(scopes))
	}
	if !r.HasScope("auth", ScopeAdmin) {
		t.Error("expected admin after overwrite")
	}
}
