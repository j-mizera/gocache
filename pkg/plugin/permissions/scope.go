package permissions

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Scope represents a permission scope granted to a plugin.
type Scope string

const (
	ScopeRead     Scope = "read"
	ScopeWrite    Scope = "write"
	ScopeAdmin    Scope = "admin"
	ScopeHookPre  Scope = "hook:pre"
	ScopeHookPost Scope = "hook:post"

	// Server query scopes — server:query is a wildcard for all subtopics.
	ScopeServerQuery        Scope = "server:query"
	ScopeServerQueryHealth  Scope = "server:query:health"
	ScopeServerQueryPlugins Scope = "server:query:plugins"
	ScopeServerQueryStats   Scope = "server:query:stats"
)

const keysPrefix = "keys:"

// OpType classifies a cache operation for scope checking.
type OpType int

const (
	OpRead OpType = iota
	OpWrite
	OpAdmin
)

// isServerQueryScope returns true if the scope is any server:query variant.
func isServerQueryScope(s Scope) bool {
	return s == ScopeServerQuery || strings.HasPrefix(string(s), "server:query:")
}

// ScopeForTopic returns the required scope for a server query topic.
func ScopeForTopic(topic string) Scope {
	return Scope("server:query:" + topic)
}

// IsKeyScope returns true if the scope is a key namespace restriction.
func IsKeyScope(s Scope) bool {
	return strings.HasPrefix(string(s), keysPrefix)
}

// KeyPattern extracts the glob pattern from a keys: scope.
// Returns "" if not a key scope.
func KeyPattern(s Scope) string {
	if !IsKeyScope(s) {
		return ""
	}
	return string(s)[len(keysPrefix):]
}

// ParseScope validates and normalizes a scope string.
func ParseScope(s string) (Scope, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "", fmt.Errorf("empty scope")
	}

	switch Scope(s) {
	case ScopeRead, ScopeWrite, ScopeAdmin, ScopeHookPre, ScopeHookPost,
		ScopeServerQuery, ScopeServerQueryHealth, ScopeServerQueryPlugins, ScopeServerQueryStats:
		return Scope(s), nil
	}

	// Accept future server:query:<topic> scopes without hardcoding every topic.
	if strings.HasPrefix(s, "server:query:") && len(s) > len("server:query:") {
		return Scope(s), nil
	}

	if strings.HasPrefix(s, keysPrefix) {
		pattern := s[len(keysPrefix):]
		if pattern == "" {
			return "", fmt.Errorf("keys: scope requires a pattern")
		}
		// Validate the glob pattern by testing it.
		if _, err := filepath.Match(pattern, "test"); err != nil {
			return "", fmt.Errorf("invalid keys pattern %q: %w", pattern, err)
		}
		return Scope(s), nil
	}

	return "", fmt.Errorf("unknown scope %q", s)
}

// ParseScopes validates and parses a list of scope strings.
func ParseScopes(ss []string) ([]Scope, error) {
	scopes := make([]Scope, 0, len(ss))
	for _, s := range ss {
		scope, err := ParseScope(s)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, nil
}

// Implies returns true if having `have` satisfies the requirement for `need`.
// Hierarchy: admin > write > read. Admin also implies server:query.
// Prefix scopes: server:query implies all server:query:* subtopics.
// Hook and key scopes are independent.
func Implies(have, need Scope) bool {
	if have == need {
		return true
	}
	switch have {
	case ScopeAdmin:
		return need == ScopeWrite || need == ScopeRead || isServerQueryScope(need)
	case ScopeWrite:
		return need == ScopeRead
	}
	// Prefix matching: server:query implies server:query:health, etc.
	h, n := string(have), string(need)
	if strings.HasSuffix(h, ":") == false && strings.HasPrefix(n, h+":") {
		return true
	}
	return false
}

// ValidateRequest computes the granted and denied scope sets.
// A requested scope is granted if any allowed scope implies it.
// Key scopes are matched exactly.
func ValidateRequest(requested, allowed []Scope) (granted, denied []Scope) {
	for _, req := range requested {
		if isGranted(req, allowed) {
			granted = append(granted, req)
		} else {
			denied = append(denied, req)
		}
	}
	return granted, denied
}

func isGranted(req Scope, allowed []Scope) bool {
	for _, a := range allowed {
		if Implies(a, req) {
			return true
		}
		// Key scopes: exact match only.
		if IsKeyScope(req) && IsKeyScope(a) && req == a {
			return true
		}
	}
	return false
}

// MatchesKey returns true if a key matches the keys: scope pattern.
func MatchesKey(scope Scope, key string) bool {
	pattern := KeyPattern(scope)
	if pattern == "" {
		return false
	}
	matched, _ := filepath.Match(pattern, key)
	return matched
}

// DefaultScopes returns the default scopes for plugins without explicit config.
func DefaultScopes() []Scope {
	return []Scope{ScopeRead}
}

// ScopeStrings converts a slice of Scope to strings (for proto serialization).
func ScopeStrings(scopes []Scope) []string {
	ss := make([]string, len(scopes))
	for i, s := range scopes {
		ss[i] = string(s)
	}
	return ss
}

// RequiredScope returns the scope needed for a given operation type.
func RequiredScope(op OpType) Scope {
	switch op {
	case OpWrite:
		return ScopeWrite
	case OpAdmin:
		return ScopeAdmin
	default:
		return ScopeRead
	}
}
