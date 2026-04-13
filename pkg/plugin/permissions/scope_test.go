package permissions

import "testing"

func TestParseScope(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Scope
		wantErr bool
	}{
		{"read", "read", ScopeRead, false},
		{"write", "write", ScopeWrite, false},
		{"admin", "admin", ScopeAdmin, false},
		{"hook:pre", "hook:pre", ScopeHookPre, false},
		{"hook:post", "hook:post", ScopeHookPost, false},
		{"keys pattern", "keys:user:*", Scope("keys:user:*"), false},
		{"keys exact", "keys:session", Scope("keys:session"), false},
		{"uppercase normalized", "READ", ScopeRead, false},
		{"mixed case", "Hook:Pre", ScopeHookPre, false},
		{"trimmed", "  write  ", ScopeWrite, false},
		{"server:query", "server:query", ScopeServerQuery, false},
		{"server:query:health", "server:query:health", ScopeServerQueryHealth, false},
		{"server:query:plugins", "server:query:plugins", ScopeServerQueryPlugins, false},
		{"server:query:stats", "server:query:stats", ScopeServerQueryStats, false},
		{"server:query:custom future topic", "server:query:custom", Scope("server:query:custom"), false},
		{"empty", "", "", true},
		{"unknown", "execute", "", true},
		{"keys empty pattern", "keys:", "", true},
		{"keys bad glob", "keys:[invalid", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScope(tt.input)
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
		scopes, err := ParseScopes([]string{"read", "write", "keys:user:*"})
		if err != nil {
			t.Fatal(err)
		}
		if len(scopes) != 3 {
			t.Errorf("expected 3 scopes, got %d", len(scopes))
		}
	})

	t.Run("error stops", func(t *testing.T) {
		_, err := ParseScopes([]string{"read", "bogus"})
		if err == nil {
			t.Error("expected error")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		scopes, err := ParseScopes(nil)
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
		have Scope
		need Scope
		want bool
	}{
		{"same scope", ScopeRead, ScopeRead, true},
		{"admin implies write", ScopeAdmin, ScopeWrite, true},
		{"admin implies read", ScopeAdmin, ScopeRead, true},
		{"write implies read", ScopeWrite, ScopeRead, true},
		{"write does not imply admin", ScopeWrite, ScopeAdmin, false},
		{"read does not imply write", ScopeRead, ScopeWrite, false},
		{"read does not imply admin", ScopeRead, ScopeAdmin, false},
		{"hook:pre is independent", ScopeHookPre, ScopeRead, false},
		{"read does not imply hook:pre", ScopeRead, ScopeHookPre, false},
		{"admin does not imply hook:pre", ScopeAdmin, ScopeHookPre, false},
		{"hook:pre same", ScopeHookPre, ScopeHookPre, true},
		{"hook:post same", ScopeHookPost, ScopeHookPost, true},
		{"hook:pre != hook:post", ScopeHookPre, ScopeHookPost, false},
		// server:query scope hierarchy
		{"admin implies server:query", ScopeAdmin, ScopeServerQuery, true},
		{"admin implies server:query:health", ScopeAdmin, ScopeServerQueryHealth, true},
		{"server:query implies server:query:health", ScopeServerQuery, ScopeServerQueryHealth, true},
		{"server:query implies server:query:plugins", ScopeServerQuery, ScopeServerQueryPlugins, true},
		{"server:query implies server:query:stats", ScopeServerQuery, ScopeServerQueryStats, true},
		{"server:query:health same", ScopeServerQueryHealth, ScopeServerQueryHealth, true},
		{"server:query:health does not imply server:query:stats", ScopeServerQueryHealth, ScopeServerQueryStats, false},
		{"read does not imply server:query", ScopeRead, ScopeServerQuery, false},
		{"hook:post does not imply server:query", ScopeHookPost, ScopeServerQuery, false},
		{"server:query:health does not imply server:query", ScopeServerQueryHealth, ScopeServerQuery, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Implies(tt.have, tt.need)
			if got != tt.want {
				t.Errorf("Implies(%q, %q) = %v, want %v", tt.have, tt.need, got, tt.want)
			}
		})
	}
}

func TestValidateRequest(t *testing.T) {
	tests := []struct {
		name        string
		requested   []Scope
		allowed     []Scope
		wantGranted int
		wantDenied  int
	}{
		{
			"all allowed",
			[]Scope{ScopeRead, ScopeWrite},
			[]Scope{ScopeAdmin},
			2, 0,
		},
		{
			"partial denied",
			[]Scope{ScopeWrite, ScopeHookPre},
			[]Scope{ScopeRead},
			0, 2,
		},
		{
			"mixed",
			[]Scope{ScopeRead, ScopeAdmin},
			[]Scope{ScopeWrite},
			1, 1, // read granted (write implies read), admin denied
		},
		{
			"key scope exact match",
			[]Scope{Scope("keys:user:*")},
			[]Scope{Scope("keys:user:*")},
			1, 0,
		},
		{
			"key scope mismatch",
			[]Scope{Scope("keys:admin:*")},
			[]Scope{Scope("keys:user:*")},
			0, 1,
		},
		{
			"hook scopes independent",
			[]Scope{ScopeHookPre, ScopeHookPost},
			[]Scope{ScopeHookPre},
			1, 1,
		},
		{
			"empty request",
			nil,
			[]Scope{ScopeAdmin},
			0, 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			granted, denied := ValidateRequest(tt.requested, tt.allowed)
			if len(granted) != tt.wantGranted {
				t.Errorf("granted: got %d (%v), want %d", len(granted), granted, tt.wantGranted)
			}
			if len(denied) != tt.wantDenied {
				t.Errorf("denied: got %d (%v), want %d", len(denied), denied, tt.wantDenied)
			}
		})
	}
}

func TestMatchesKey(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		key   string
		want  bool
	}{
		{"wildcard match", Scope("keys:user:*"), "user:123", true},
		{"wildcard no match", Scope("keys:user:*"), "session:abc", false},
		{"exact match", Scope("keys:config"), "config", true},
		{"exact no match", Scope("keys:config"), "other", false},
		{"not a key scope", ScopeRead, "anything", false},
		{"nested wildcard", Scope("keys:kafka:*"), "kafka:events", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesKey(tt.scope, tt.key)
			if got != tt.want {
				t.Errorf("MatchesKey(%q, %q) = %v, want %v", tt.scope, tt.key, got, tt.want)
			}
		})
	}
}

func TestIsKeyScope(t *testing.T) {
	if !IsKeyScope(Scope("keys:foo:*")) {
		t.Error("expected keys:foo:* to be a key scope")
	}
	if IsKeyScope(ScopeRead) {
		t.Error("expected read to not be a key scope")
	}
}

func TestKeyPattern(t *testing.T) {
	if got := KeyPattern(Scope("keys:user:*")); got != "user:*" {
		t.Errorf("got %q, want %q", got, "user:*")
	}
	if got := KeyPattern(ScopeRead); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDefaultScopes(t *testing.T) {
	d := DefaultScopes()
	if len(d) != 1 || d[0] != ScopeRead {
		t.Errorf("expected [read], got %v", d)
	}
}

func TestScopeStrings(t *testing.T) {
	ss := ScopeStrings([]Scope{ScopeRead, ScopeWrite})
	if len(ss) != 2 || ss[0] != "read" || ss[1] != "write" {
		t.Errorf("unexpected: %v", ss)
	}
}

func TestRequiredScope(t *testing.T) {
	if RequiredScope(OpRead) != ScopeRead {
		t.Error("OpRead should require ScopeRead")
	}
	if RequiredScope(OpWrite) != ScopeWrite {
		t.Error("OpWrite should require ScopeWrite")
	}
	if RequiredScope(OpAdmin) != ScopeAdmin {
		t.Error("OpAdmin should require ScopeAdmin")
	}
}
