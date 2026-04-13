package context

import "testing"

func TestIsSecret(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		// Server namespace
		{"_start_ns", false},
		{"_operation_id", false},
		{"_secret.session_token", true},
		{"_secret.jwt", true},

		// Plugin-private namespace
		{"auth.cache_hit", false},
		{"auth.secret.api_key", true},
		{"gobservability.span_id", false},
		{"gobservability.secret.internal", true},

		// Shared namespace
		{"shared.username", false},
		{"shared.traceparent", false},
		{"shared.secret.accountId", true},
		{"shared.secret.jwt", true},

		// Bare secret prefix
		{"secret.something", true},

		// Edge cases
		{"", false},
		{"nosecrethere", false},
		{"my_secret_key", false}, // no dot boundaries → not a secret
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := IsSecret(tt.key); got != tt.want {
				t.Errorf("IsSecret(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestFilterForPlugin(t *testing.T) {
	ctx := map[string]string{
		"_start_ns":                  "123",
		"_operation_id":              "cmd_1",
		"_secret.session":            "tok",
		"auth.cache_hit":             "true",
		"auth.secret.api_key":        "key123",
		"gobservability.span_id":     "span1",
		"gobservability.secret.data": "hidden",
		"shared.username":            "john",
		"shared.secret.jwt":          "eyJ...",
	}

	t.Run("auth sees own + server + shared", func(t *testing.T) {
		filtered := FilterForPlugin(ctx, "auth")
		// Should see: _*, auth.*, shared.*
		expect := map[string]bool{
			"_start_ns":           true,
			"_operation_id":       true,
			"_secret.session":     true,
			"auth.cache_hit":      true,
			"auth.secret.api_key": true,
			"shared.username":     true,
			"shared.secret.jwt":   true,
		}
		for k := range expect {
			if _, ok := filtered[k]; !ok {
				t.Errorf("expected key %q in auth filtered context", k)
			}
		}
		// Should NOT see gobservability.*
		if _, ok := filtered["gobservability.span_id"]; ok {
			t.Error("auth should not see gobservability.span_id")
		}
		if _, ok := filtered["gobservability.secret.data"]; ok {
			t.Error("auth should not see gobservability.secret.data")
		}
	})

	t.Run("gobservability sees own + server + shared", func(t *testing.T) {
		filtered := FilterForPlugin(ctx, "gobservability")
		if _, ok := filtered["gobservability.span_id"]; !ok {
			t.Error("gobservability should see own span_id")
		}
		if _, ok := filtered["auth.cache_hit"]; ok {
			t.Error("gobservability should not see auth.cache_hit")
		}
	})

	t.Run("empty ctx returns nil", func(t *testing.T) {
		if FilterForPlugin(nil, "auth") != nil {
			t.Error("expected nil for nil ctx")
		}
		if FilterForPlugin(map[string]string{}, "auth") != nil {
			t.Error("expected nil for empty ctx")
		}
	})

	t.Run("no matching keys returns nil", func(t *testing.T) {
		result := FilterForPlugin(map[string]string{"other.key": "val"}, "auth")
		if result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})
}

func TestMergeFromPlugin(t *testing.T) {
	t.Run("auto-prefix non-shared keys", func(t *testing.T) {
		ctx := NewContext()
		MergeFromPlugin(ctx, "auth", map[string]string{
			"cache_hit": "true",
			"token_age": "300",
		})
		if ctx["auth.cache_hit"] != "true" {
			t.Errorf("expected auto-prefixed key, got %v", ctx)
		}
		if ctx["auth.token_age"] != "300" {
			t.Errorf("expected auto-prefixed key, got %v", ctx)
		}
	})

	t.Run("shared keys kept as-is", func(t *testing.T) {
		ctx := NewContext()
		MergeFromPlugin(ctx, "auth", map[string]string{
			"shared.username":         "john",
			"shared.secret.accountId": "acc-789",
		})
		if ctx["shared.username"] != "john" {
			t.Error("shared key should not be prefixed")
		}
		if ctx["shared.secret.accountId"] != "acc-789" {
			t.Error("shared secret key should not be prefixed")
		}
	})

	t.Run("secret keys auto-prefix correctly", func(t *testing.T) {
		ctx := NewContext()
		MergeFromPlugin(ctx, "auth", map[string]string{
			"secret.api_key": "key123",
		})
		if ctx["auth.secret.api_key"] != "key123" {
			t.Errorf("expected auth.secret.api_key, got %v", ctx)
		}
		if !IsSecret("auth.secret.api_key") {
			t.Error("prefixed secret key should still be detected as secret")
		}
	})
}

func TestRedactSecrets(t *testing.T) {
	ctx := map[string]string{
		"_start_ns":              "123",
		"_secret.session":        "tok",
		"shared.username":        "john",
		"shared.secret.jwt":      "eyJ...",
		"auth.cache_hit":         "true",
		"auth.secret.api_key":    "key123",
		"gobservability.span_id": "span1",
	}

	redacted := RedactSecrets(ctx)

	// Should keep non-secret keys
	if redacted["_start_ns"] != "123" {
		t.Error("expected _start_ns to survive redaction")
	}
	if redacted["shared.username"] != "john" {
		t.Error("expected shared.username to survive redaction")
	}
	if redacted["auth.cache_hit"] != "true" {
		t.Error("expected auth.cache_hit to survive redaction")
	}

	// Should strip secret keys
	if _, ok := redacted["_secret.session"]; ok {
		t.Error("_secret.session should be redacted")
	}
	if _, ok := redacted["shared.secret.jwt"]; ok {
		t.Error("shared.secret.jwt should be redacted")
	}
	if _, ok := redacted["auth.secret.api_key"]; ok {
		t.Error("auth.secret.api_key should be redacted")
	}

	// Original unchanged
	if ctx["_secret.session"] != "tok" {
		t.Error("original should not be modified")
	}
}

func TestRedactSecrets_Empty(t *testing.T) {
	if RedactSecrets(nil) != nil {
		t.Error("nil input should return nil")
	}
	if RedactSecrets(map[string]string{}) != nil {
		t.Error("empty input should return nil")
	}
}

func TestNewContext(t *testing.T) {
	ctx := NewContext()
	if ctx == nil {
		t.Error("NewContext should return non-nil map")
	}
	if len(ctx) != 0 {
		t.Error("NewContext should return empty map")
	}
}
