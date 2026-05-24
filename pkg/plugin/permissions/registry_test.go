package permissions

import (
	"testing"

	"gocache/api/scope"
)

func TestRegistryBasic(t *testing.T) {
	r := NewRegistry()

	if got := r.GetScopes("unknown"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if r.HasScope("unknown", scope.ScopeRead) {
		t.Error("expected false for unknown plugin")
	}

	r.Register("auth", []scope.Scope{scope.ScopeHookPre, scope.ScopeRead})
	scopes := r.GetScopes("auth")
	if len(scopes) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(scopes))
	}
	if !r.HasScope("auth", scope.ScopeRead) {
		t.Error("expected auth to have read scope")
	}
	if !r.HasScope("auth", scope.ScopeHookPre) {
		t.Error("expected auth to have hook:pre scope")
	}
	if r.HasScope("auth", scope.ScopeWrite) {
		t.Error("expected auth to not have write scope")
	}
}

func TestRegistryHierarchy(t *testing.T) {
	r := NewRegistry()
	r.Register("kafka", []scope.Scope{scope.ScopeWrite})

	if !r.HasScope("kafka", scope.ScopeRead) {
		t.Error("write should imply read")
	}
	if !r.HasScope("kafka", scope.ScopeWrite) {
		t.Error("should have write directly")
	}
	if r.HasScope("kafka", scope.ScopeAdmin) {
		t.Error("write should not imply admin")
	}
}

func TestRegistryAdminImpliesAll(t *testing.T) {
	r := NewRegistry()
	r.Register("cluster", []scope.Scope{scope.ScopeAdmin})

	if !r.HasScope("cluster", scope.ScopeRead) {
		t.Error("admin should imply read")
	}
	if !r.HasScope("cluster", scope.ScopeWrite) {
		t.Error("admin should imply write")
	}
	if !r.HasScope("cluster", scope.ScopeAdmin) {
		t.Error("admin should have admin")
	}
	if r.HasScope("cluster", scope.ScopeHookPre) {
		t.Error("admin should not imply hook:pre")
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := NewRegistry()
	r.Register("metrics", []scope.Scope{scope.ScopeRead, scope.ScopeHookPost})

	if !r.HasScope("metrics", scope.ScopeRead) {
		t.Fatal("expected read scope")
	}

	r.Unregister("metrics")

	if r.HasScope("metrics", scope.ScopeRead) {
		t.Error("expected no scopes after unregister")
	}
	if got := r.GetScopes("metrics"); got != nil {
		t.Errorf("expected nil after unregister, got %v", got)
	}
}

func TestRegistryKeyScopes(t *testing.T) {
	r := NewRegistry()
	r.Register("kafka", []scope.Scope{scope.ScopeWrite, scope.Scope("keys:kafka:*"), scope.Scope("keys:events:*")})

	ks := r.KeyScopes("kafka")
	if len(ks) != 2 {
		t.Errorf("expected 2 key scopes, got %d", len(ks))
	}

	r.Register("metrics", []scope.Scope{scope.ScopeRead})
	ks2 := r.KeyScopes("metrics")
	if len(ks2) != 0 {
		t.Errorf("expected 0 key scopes, got %d", len(ks2))
	}
}

func TestRegistryOverwrite(t *testing.T) {
	r := NewRegistry()
	r.Register("auth", []scope.Scope{scope.ScopeRead})
	r.Register("auth", []scope.Scope{scope.ScopeAdmin, scope.ScopeHookPre})

	scopes := r.GetScopes("auth")
	if len(scopes) != 2 {
		t.Errorf("expected 2 scopes after overwrite, got %d", len(scopes))
	}
	if !r.HasScope("auth", scope.ScopeAdmin) {
		t.Error("expected admin after overwrite")
	}
}
