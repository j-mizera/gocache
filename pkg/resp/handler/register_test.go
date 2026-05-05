package handler

import (
	"testing"
)

// TestRegistrations_ShardingClassification verifies every registered
// command falls into exactly one of three sharding buckets:
//   - keyless: KeyArgIndex == -1, MultiKey == false
//   - single-key: KeyArgIndex >= 0, MultiKey == false
//   - multi-key: MultiKey == true (KeyArgIndex ignored)
//
// And that single-key commands' KeyArgIndex is within [0, Min) — i.e.,
// the key is at a position the command actually requires. This catches
// off-by-one mistakes when a future command adds args before the key.
func TestRegistrations_ShardingClassification(t *testing.T) {
	regs := Registrations()
	if len(regs) == 0 {
		t.Fatal("Registrations() returned empty map")
	}

	for name, r := range regs {
		s := r.Spec
		switch {
		case s.MultiKey:
			// MultiKey overrides KeyArgIndex; nothing else to check.
		case s.KeyArgIndex == -1:
			// Keyless. Min/Max can be any non-negative pair.
		case s.KeyArgIndex >= 0:
			// Single-key. KeyArgIndex must be a position the command
			// actually requires (i.e. < Min).
			if s.Min <= s.KeyArgIndex {
				t.Errorf("%s: single-key but KeyArgIndex=%d >= Min=%d (key not guaranteed present)",
					name, s.KeyArgIndex, s.Min)
			}
		default:
			t.Errorf("%s: invalid Spec.KeyArgIndex=%d (must be -1 or >=0)", name, s.KeyArgIndex)
		}
	}
}

// TestRegistrations_KeylessCommands enumerates the expected keyless set.
// If a future change reclassifies one of these, the test will surface
// the change so the sharded engine routing is reconsidered.
func TestRegistrations_KeylessCommands(t *testing.T) {
	expected := map[string]bool{
		"MULTI":    true,
		"DISCARD":  true,
		"AUTH":     true,
		"HELLO":    true,
		"PING":     true,
		"ECHO":     true,
		"SELECT":   true,
		"INFO":     true,
		"UNWATCH":  true,
		"BGSAVE":   true,
		"LASTSAVE": true,
	}
	regs := Registrations()
	for name, r := range regs {
		isKeyless := r.Spec.KeyArgIndex == -1 && !r.Spec.MultiKey
		want := expected[name]
		if isKeyless != want {
			t.Errorf("%s: keyless=%v, want %v", name, isKeyless, want)
		}
	}
}
