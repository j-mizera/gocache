package permissions

import (
	"testing"

	"gocache/api/scope"
)

func TestEnforcerCheck(t *testing.T) {
	r := NewRegistry()
	e := NewEnforcer(r)

	r.Register("kafka", []scope.Scope{scope.ScopeWrite, scope.Scope("keys:kafka:*")})

	tests := []struct {
		name    string
		plugin  string
		op      scope.OpType
		keys    []string
		wantErr bool
	}{
		{"write allowed", "kafka", scope.OpWrite, []string{"kafka:events"}, false},
		{"read allowed (write implies)", "kafka", scope.OpRead, []string{"kafka:events"}, false},
		{"admin denied", "kafka", scope.OpAdmin, nil, true},
		{"key outside namespace", "kafka", scope.OpWrite, []string{"user:123"}, true},
		{"key inside namespace", "kafka", scope.OpWrite, []string{"kafka:logs"}, false},
		{"empty keys ok", "kafka", scope.OpWrite, nil, false},
		{"empty key string skipped", "kafka", scope.OpWrite, []string{""}, false},
		{"unknown plugin", "unknown", scope.OpRead, nil, true},
		{"multiple keys all valid", "kafka", scope.OpWrite, []string{"kafka:a", "kafka:b"}, false},
		{"multiple keys one invalid", "kafka", scope.OpWrite, []string{"kafka:a", "other:b"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := e.Check(tt.plugin, tt.op, tt.keys)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestEnforcerNoKeyRestriction(t *testing.T) {
	r := NewRegistry()
	e := NewEnforcer(r)

	r.Register("pubsub", []scope.Scope{scope.ScopeWrite})

	if err := e.Check("pubsub", scope.OpWrite, []string{"any:key", "other:key"}); err != nil {
		t.Errorf("expected no restriction without key scopes: %v", err)
	}
}

func TestEnforcerMultipleKeyPatterns(t *testing.T) {
	r := NewRegistry()
	e := NewEnforcer(r)

	r.Register("multi", []scope.Scope{scope.ScopeRead, scope.Scope("keys:user:*"), scope.Scope("keys:session:*")})

	if err := e.Check("multi", scope.OpRead, []string{"user:123"}); err != nil {
		t.Errorf("user key should match: %v", err)
	}
	if err := e.Check("multi", scope.OpRead, []string{"session:abc"}); err != nil {
		t.Errorf("session key should match: %v", err)
	}
	if err := e.Check("multi", scope.OpRead, []string{"admin:secret"}); err == nil {
		t.Error("admin key should not match")
	}
}
