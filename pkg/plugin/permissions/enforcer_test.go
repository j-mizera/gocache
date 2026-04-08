package permissions

import "testing"

func TestEnforcerCheck(t *testing.T) {
	r := NewRegistry()
	e := NewEnforcer(r)

	// Plugin with write + key restriction.
	r.Register("kafka", []Scope{ScopeWrite, Scope("keys:kafka:*")})

	tests := []struct {
		name    string
		plugin  string
		op      OpType
		keys    []string
		wantErr bool
	}{
		{"write allowed", "kafka", OpWrite, []string{"kafka:events"}, false},
		{"read allowed (write implies)", "kafka", OpRead, []string{"kafka:events"}, false},
		{"admin denied", "kafka", OpAdmin, nil, true},
		{"key outside namespace", "kafka", OpWrite, []string{"user:123"}, true},
		{"key inside namespace", "kafka", OpWrite, []string{"kafka:logs"}, false},
		{"empty keys ok", "kafka", OpWrite, nil, false},
		{"empty key string skipped", "kafka", OpWrite, []string{""}, false},
		{"unknown plugin", "unknown", OpRead, nil, true},
		{"multiple keys all valid", "kafka", OpWrite, []string{"kafka:a", "kafka:b"}, false},
		{"multiple keys one invalid", "kafka", OpWrite, []string{"kafka:a", "other:b"}, true},
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

	// Plugin with write but no key restrictions — all keys allowed.
	r.Register("pubsub", []Scope{ScopeWrite})

	if err := e.Check("pubsub", OpWrite, []string{"any:key", "other:key"}); err != nil {
		t.Errorf("expected no restriction without key scopes: %v", err)
	}
}

func TestEnforcerMultipleKeyPatterns(t *testing.T) {
	r := NewRegistry()
	e := NewEnforcer(r)

	// Plugin with multiple key patterns — key must match at least one.
	r.Register("multi", []Scope{ScopeRead, Scope("keys:user:*"), Scope("keys:session:*")})

	if err := e.Check("multi", OpRead, []string{"user:123"}); err != nil {
		t.Errorf("user key should match: %v", err)
	}
	if err := e.Check("multi", OpRead, []string{"session:abc"}); err != nil {
		t.Errorf("session key should match: %v", err)
	}
	if err := e.Check("multi", OpRead, []string{"admin:secret"}); err == nil {
		t.Error("admin key should not match")
	}
}
