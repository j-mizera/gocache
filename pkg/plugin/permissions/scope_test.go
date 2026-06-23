package permissions

import (
	"testing"

	"gocache/api/scope"
)

func TestParseScope(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    scope.Scope
		wantErr bool
	}{
		{"read", "read", scope.ScopeRead, false},
		{"write", "write", scope.ScopeWrite, false},
		{"admin", "admin", scope.ScopeAdmin, false},
		{"hook:pre", "hook:pre", scope.ScopeHookPre, false},
		{"hook:post", "hook:post", scope.ScopeHookPost, false},
		{"keys pattern", "keys:user:*", scope.Scope("keys:user:*"), false},
		{"keys exact", "keys:session", scope.Scope("keys:session"), false},
		{"uppercase normalized", "READ", scope.ScopeRead, false},
		{"mixed case", "Hook:Pre", scope.ScopeHookPre, false},
		{"trimmed", "  write  ", scope.ScopeWrite, false},
		{"server:query", "server:query", scope.ScopeServerQuery, false},
		{"server:query:health", "server:query:health", scope.ScopeServerQueryHealth, false},
		{"server:query:plugins", "server:query:plugins", scope.ScopeServerQueryPlugins, false},
		{"server:query:stats", "server:query:stats", scope.ScopeServerQueryStats, false},
		{"server:query:metrics.commands", "server:query:metrics.commands", scope.ScopeServerQueryMetricsCommands, false},
		{"server:query:metrics.telemetry", "server:query:metrics.telemetry", scope.ScopeServerQueryMetricsTelemetry, false},
		{"server:query:custom future topic", "server:query:custom", scope.Scope("server:query:custom"), false},
		{"empty", "", "", true},
		{"unknown", "execute", "", true},
		{"keys empty pattern", "keys:", "", true},
		{"keys bad glob", "keys:[invalid", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scope.ParseScope(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseScopes(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		scopes, err := scope.ParseScopes([]string{"read", "write", "keys:user:*"})
		if err != nil {
			t.Fatal(err)
		}
		if len(scopes) != 3 {
			t.Errorf("expected 3 scopes, got %d", len(scopes))
		}
	})

	t.Run("error stops", func(t *testing.T) {
		_, err := scope.ParseScopes([]string{"read", "bogus"})
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		scopes, err := scope.ParseScopes(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(scopes) != 0 {
			t.Errorf("expected 0 scopes, got %d", len(scopes))
		}
	})
}

func TestImplies(t *testing.T) {
	tests := []struct {
		name string
		have scope.Scope
		need scope.Scope
		want bool
	}{
		{"same scope", scope.ScopeRead, scope.ScopeRead, true},
		{"admin implies write", scope.ScopeAdmin, scope.ScopeWrite, true},
		{"admin implies read", scope.ScopeAdmin, scope.ScopeRead, true},
		{"write implies read", scope.ScopeWrite, scope.ScopeRead, true},
		{"write does not imply admin", scope.ScopeWrite, scope.ScopeAdmin, false},
		{"read does not imply write", scope.ScopeRead, scope.ScopeWrite, false},
		{"read does not imply admin", scope.ScopeRead, scope.ScopeAdmin, false},
		{"hook:pre is independent", scope.ScopeHookPre, scope.ScopeRead, false},
		{"read does not imply hook:pre", scope.ScopeRead, scope.ScopeHookPre, false},
		{"admin does not imply hook:pre", scope.ScopeAdmin, scope.ScopeHookPre, false},
		{"hook:pre same", scope.ScopeHookPre, scope.ScopeHookPre, true},
		{"hook:post same", scope.ScopeHookPost, scope.ScopeHookPost, true},
		{"hook:pre != hook:post", scope.ScopeHookPre, scope.ScopeHookPost, false},
		// server:query scope hierarchy
		{"admin implies server:query", scope.ScopeAdmin, scope.ScopeServerQuery, true},
		{"admin implies server:query:health", scope.ScopeAdmin, scope.ScopeServerQueryHealth, true},
		{"server:query implies server:query:health", scope.ScopeServerQuery, scope.ScopeServerQueryHealth, true},
		{"server:query implies server:query:plugins", scope.ScopeServerQuery, scope.ScopeServerQueryPlugins, true},
		{"server:query implies server:query:stats", scope.ScopeServerQuery, scope.ScopeServerQueryStats, true},
		{"server:query implies server:query:metrics.telemetry", scope.ScopeServerQuery, scope.ScopeServerQueryMetricsTelemetry, true},
		{"admin implies server:query:metrics.telemetry", scope.ScopeAdmin, scope.ScopeServerQueryMetricsTelemetry, true},
		{"server:query:health same", scope.ScopeServerQueryHealth, scope.ScopeServerQueryHealth, true},
		{"server:query:health does not imply server:query:stats", scope.ScopeServerQueryHealth, scope.ScopeServerQueryStats, false},
		{"read does not imply server:query", scope.ScopeRead, scope.ScopeServerQuery, false},
		{"hook:post does not imply server:query", scope.ScopeHookPost, scope.ScopeServerQuery, false},
		{"server:query:health does not imply server:query", scope.ScopeServerQueryHealth, scope.ScopeServerQuery, false},
		{"server:query:metrics.telemetry does not imply server:query:metrics.commands", scope.ScopeServerQueryMetricsTelemetry, scope.ScopeServerQueryMetricsCommands, false},
		{"server:query:metrics.commands does not imply server:query:metrics.telemetry", scope.ScopeServerQueryMetricsCommands, scope.ScopeServerQueryMetricsTelemetry, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scope.Implies(tt.have, tt.need)
			if got != tt.want {
				t.Errorf("Implies(%q, %q) = %v, want %v", tt.have, tt.need, got, tt.want)
			}
		})
	}
}

func TestValidateRequest(t *testing.T) {
	tests := []struct {
		name        string
		requested   []scope.Scope
		allowed     []scope.Scope
		wantGranted []scope.Scope
		wantDenied  []scope.Scope
	}{
		{
			"all allowed",
			[]scope.Scope{scope.ScopeRead, scope.ScopeWrite},
			[]scope.Scope{scope.ScopeAdmin},
			[]scope.Scope{scope.ScopeRead, scope.ScopeWrite}, nil,
		},
		{
			"partial denied",
			[]scope.Scope{scope.ScopeWrite, scope.ScopeHookPre},
			[]scope.Scope{scope.ScopeRead},
			nil, []scope.Scope{scope.ScopeWrite, scope.ScopeHookPre},
		},
		{
			"mixed",
			[]scope.Scope{scope.ScopeRead, scope.ScopeAdmin},
			[]scope.Scope{scope.ScopeWrite},
			[]scope.Scope{scope.ScopeRead}, []scope.Scope{scope.ScopeAdmin},
		},
		{
			"key scope exact match",
			[]scope.Scope{scope.Scope("keys:user:*")},
			[]scope.Scope{scope.Scope("keys:user:*")},
			[]scope.Scope{scope.Scope("keys:user:*")}, nil,
		},
		{
			"key scope mismatch",
			[]scope.Scope{scope.Scope("keys:admin:*")},
			[]scope.Scope{scope.Scope("keys:user:*")},
			nil, []scope.Scope{scope.Scope("keys:admin:*")},
		},
		{
			"hook scopes independent",
			[]scope.Scope{scope.ScopeHookPre, scope.ScopeHookPost},
			[]scope.Scope{scope.ScopeHookPre},
			[]scope.Scope{scope.ScopeHookPre}, []scope.Scope{scope.ScopeHookPost},
		},
		{
			"server query wildcard allows telemetry",
			[]scope.Scope{scope.ScopeServerQueryMetricsTelemetry},
			[]scope.Scope{scope.ScopeServerQuery},
			[]scope.Scope{scope.ScopeServerQueryMetricsTelemetry}, nil,
		},
		{
			"exact telemetry denies metrics commands",
			[]scope.Scope{scope.ScopeServerQueryMetricsTelemetry, scope.ScopeServerQueryMetricsCommands},
			[]scope.Scope{scope.ScopeServerQueryMetricsTelemetry},
			[]scope.Scope{scope.ScopeServerQueryMetricsTelemetry}, []scope.Scope{scope.ScopeServerQueryMetricsCommands},
		},
		{
			"exact metrics commands denies telemetry",
			[]scope.Scope{scope.ScopeServerQueryMetricsCommands, scope.ScopeServerQueryMetricsTelemetry},
			[]scope.Scope{scope.ScopeServerQueryMetricsCommands},
			[]scope.Scope{scope.ScopeServerQueryMetricsCommands}, []scope.Scope{scope.ScopeServerQueryMetricsTelemetry},
		},
		{
			"empty request",
			nil,
			[]scope.Scope{scope.ScopeAdmin},
			nil, nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			granted, denied := scope.ValidateRequest(tt.requested, tt.allowed)
			if len(granted) != len(tt.wantGranted) {
				t.Fatalf("granted: got %d (%v), want %d (%v)", len(granted), granted, len(tt.wantGranted), tt.wantGranted)
			}
			for i := range tt.wantGranted {
				if granted[i] != tt.wantGranted[i] {
					t.Errorf("granted[%d]: got %q, want %q", i, granted[i], tt.wantGranted[i])
				}
			}
			if len(denied) != len(tt.wantDenied) {
				t.Fatalf("denied: got %d (%v), want %d (%v)", len(denied), denied, len(tt.wantDenied), tt.wantDenied)
			}
			for i := range tt.wantDenied {
				if denied[i] != tt.wantDenied[i] {
					t.Errorf("denied[%d]: got %q, want %q", i, denied[i], tt.wantDenied[i])
				}
			}
		})
	}
}

func TestMatchesKey(t *testing.T) {
	tests := []struct {
		name  string
		scope scope.Scope
		key   string
		want  bool
	}{
		{"wildcard match", scope.Scope("keys:user:*"), "user:123", true},
		{"wildcard no match", scope.Scope("keys:user:*"), "session:abc", false},
		{"exact match", scope.Scope("keys:config"), "config", true},
		{"exact no match", scope.Scope("keys:config"), "other", false},
		{"not a key scope", scope.ScopeRead, "anything", false},
		{"nested wildcard", scope.Scope("keys:kafka:*"), "kafka:events", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scope.MatchesKey(tt.scope, tt.key)
			if got != tt.want {
				t.Errorf("MatchesKey(%q, %q) = %v, want %v", tt.scope, tt.key, got, tt.want)
			}
		})
	}
}

func TestIsKeyScope(t *testing.T) {
	if !scope.IsKeyScope(scope.Scope("keys:foo:*")) {
		t.Error("expected keys:foo:* to be a key scope")
	}
	if scope.IsKeyScope(scope.ScopeRead) {
		t.Error("expected read to not be a key scope")
	}
}

func TestKeyPattern(t *testing.T) {
	if got := scope.KeyPattern(scope.Scope("keys:user:*")); got != "user:*" {
		t.Errorf("got %q, want %q", got, "user:*")
	}
	if got := scope.KeyPattern(scope.ScopeRead); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDefaultScopes(t *testing.T) {
	d := scope.DefaultScopes()
	if len(d) != 1 || d[0] != scope.ScopeRead {
		t.Errorf("expected [read], got %v", d)
	}
}

func TestScopeStrings(t *testing.T) {
	ss := scope.ScopeStrings([]scope.Scope{scope.ScopeRead, scope.ScopeWrite})
	if len(ss) != 2 || ss[0] != "read" || ss[1] != "write" {
		t.Errorf("unexpected: %v", ss)
	}
}

func TestRequiredScope(t *testing.T) {
	if scope.RequiredScope(scope.OpRead) != scope.ScopeRead {
		t.Error("OpRead should require ScopeRead")
	}
	if scope.RequiredScope(scope.OpWrite) != scope.ScopeWrite {
		t.Error("OpWrite should require ScopeWrite")
	}
	if scope.RequiredScope(scope.OpAdmin) != scope.ScopeAdmin {
		t.Error("OpAdmin should require ScopeAdmin")
	}
}
