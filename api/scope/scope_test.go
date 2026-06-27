package scope

import "testing"

func TestParseScopeAcceptsTelemetry(t *testing.T) {
	got, err := ParseScope("telemetry")
	if err != nil {
		t.Fatalf("ParseScope(%q) error = %v, want nil", "telemetry", err)
	}
	if got != ScopeTelemetry {
		t.Fatalf("ParseScope(%q) = %q, want %q", "telemetry", got, ScopeTelemetry)
	}
}

func TestImpliesTelemetry(t *testing.T) {
	tests := []struct {
		name string
		have Scope
		need Scope
		want bool
	}{
		{name: "admin implies telemetry", have: ScopeAdmin, need: ScopeTelemetry, want: true},
		{name: "write does not imply telemetry", have: ScopeWrite, need: ScopeTelemetry, want: false},
		{name: "telemetry implies itself", have: ScopeTelemetry, need: ScopeTelemetry, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Implies(tt.have, tt.need)
			if got != tt.want {
				t.Fatalf("Implies(%q, %q) = %t, want %t", tt.have, tt.need, got, tt.want)
			}
		})
	}
}

func TestScopeHelpers(t *testing.T) {
	if got := ScopeForTopic("health"); got != ScopeServerQueryHealth {
		t.Fatalf("ScopeForTopic(%q) = %q, want %q", "health", got, ScopeServerQueryHealth)
	}
	if got := IsKeyScope(Scope("keys:user:*")); got != true {
		t.Fatalf("IsKeyScope(%q) = %t, want true", "keys:user:*", got)
	}
	if got := IsKeyScope(ScopeTelemetry); got != false {
		t.Fatalf("IsKeyScope(%q) = %t, want false", ScopeTelemetry, got)
	}
	if got := KeyPattern(Scope("keys:user:*")); got != "user:*" {
		t.Fatalf("KeyPattern(%q) = %q, want %q", "keys:user:*", got, "user:*")
	}
	if got := KeyPattern(ScopeRead); got != "" {
		t.Fatalf("KeyPattern(%q) = %q, want empty string", ScopeRead, got)
	}
}

func TestParseScopesAndErrors(t *testing.T) {
	got, err := ParseScopes([]string{" read ", "TELEMETRY", "server:query:custom", "keys:user:*"})
	if err != nil {
		t.Fatalf("ParseScopes valid input error = %v, want nil", err)
	}
	want := []Scope{ScopeRead, ScopeTelemetry, Scope("server:query:custom"), Scope("keys:user:*")}
	if len(got) != len(want) {
		t.Fatalf("ParseScopes valid input len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParseScopes valid input[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if _, err := ParseScope(""); err == nil || err.Error() != "empty scope" {
		t.Fatalf("ParseScope(empty) error = %v, want %q", err, "empty scope")
	}
	if _, err := ParseScope("keys:"); err == nil || err.Error() != "keys: scope requires a pattern" {
		t.Fatalf("ParseScope(keys:) error = %v, want %q", err, "keys: scope requires a pattern")
	}
	if _, err := ParseScope("unknown"); err == nil || err.Error() != "unknown scope \"unknown\"" {
		t.Fatalf("ParseScope(unknown) error = %v, want %q", err, "unknown scope \"unknown\"")
	}
}

func TestValidateRequestAndMatchesKey(t *testing.T) {
	granted, denied := ValidateRequest(
		[]Scope{ScopeRead, ScopeTelemetry, Scope("keys:user:*"), ScopeWrite},
		[]Scope{ScopeAdmin, Scope("keys:user:*")},
	)
	wantGranted := []Scope{ScopeRead, ScopeTelemetry, Scope("keys:user:*"), ScopeWrite}
	if len(granted) != len(wantGranted) {
		t.Fatalf("ValidateRequest granted len = %d, want %d", len(granted), len(wantGranted))
	}
	for i := range wantGranted {
		if granted[i] != wantGranted[i] {
			t.Fatalf("ValidateRequest granted[%d] = %q, want %q", i, granted[i], wantGranted[i])
		}
	}
	if len(denied) != 0 {
		t.Fatalf("ValidateRequest denied = %#v, want empty", denied)
	}

	granted, denied = ValidateRequest([]Scope{ScopeTelemetry}, []Scope{ScopeWrite})
	if len(granted) != 0 {
		t.Fatalf("ValidateRequest telemetry/write granted = %#v, want empty", granted)
	}
	if len(denied) != 1 || denied[0] != ScopeTelemetry {
		t.Fatalf("ValidateRequest telemetry/write denied = %#v, want [%q]", denied, ScopeTelemetry)
	}

	if got := MatchesKey(Scope("keys:user:*"), "user:42"); got != true {
		t.Fatalf("MatchesKey(%q, %q) = %t, want true", "keys:user:*", "user:42", got)
	}
	if got := MatchesKey(Scope("keys:user:*"), "order:42"); got != false {
		t.Fatalf("MatchesKey(%q, %q) = %t, want false", "keys:user:*", "order:42", got)
	}
}

func TestDefaultAndConversionHelpers(t *testing.T) {
	defaults := DefaultScopes()
	if len(defaults) != 1 || defaults[0] != ScopeRead {
		t.Fatalf("DefaultScopes() = %#v, want [%q]", defaults, ScopeRead)
	}

	strings := ScopeStrings([]Scope{ScopeRead, ScopeTelemetry})
	wantStrings := []string{"read", "telemetry"}
	if len(strings) != len(wantStrings) {
		t.Fatalf("ScopeStrings len = %d, want %d", len(strings), len(wantStrings))
	}
	for i := range wantStrings {
		if strings[i] != wantStrings[i] {
			t.Fatalf("ScopeStrings[%d] = %q, want %q", i, strings[i], wantStrings[i])
		}
	}
}

func TestRequiredScope(t *testing.T) {
	tests := []struct {
		name string
		op   OpType
		want Scope
	}{
		{name: "read", op: OpRead, want: ScopeRead},
		{name: "write", op: OpWrite, want: ScopeWrite},
		{name: "admin", op: OpAdmin, want: ScopeAdmin},
		{name: "unknown defaults read", op: OpType(99), want: ScopeRead},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RequiredScope(tt.op)
			if got != tt.want {
				t.Fatalf("RequiredScope(%d) = %q, want %q", tt.op, got, tt.want)
			}
		})
	}
}
