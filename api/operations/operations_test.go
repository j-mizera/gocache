package operations

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNew_UniqueIDs(t *testing.T) {
	ids := make(map[string]struct{})
	for range 100 {
		op := New(TypeCommand, "")
		if _, exists := ids[op.ID]; exists {
			t.Fatalf("duplicate ID: %s", op.ID)
		}
		ids[op.ID] = struct{}{}
	}
}

func TestNew_IDPrefix(t *testing.T) {
	tests := []struct {
		opType Type
		prefix string
	}{
		{TypeCommand, "cmd_"},
		{TypeConnection, "conn_"},
		{TypeCleanup, "cleanup_"},
		{TypeSnapshot, "snap_"},
		{TypeStartup, "boot_"},
		{TypeConfigReload, "cfg_"},
		{TypePluginStart, "pstart_"},
		{TypePluginStop, "pstop_"},
	}

	for _, tt := range tests {
		t.Run(string(tt.opType), func(t *testing.T) {
			op := New(tt.opType, "")
			if !strings.HasPrefix(op.ID, tt.prefix) {
				t.Errorf("expected prefix %q, got ID %q", tt.prefix, op.ID)
			}
		})
	}
}

func TestNew_ConcurrentIDGeneration(t *testing.T) {
	var wg sync.WaitGroup
	ids := sync.Map{}
	const n = 200

	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := New(TypeCommand, "")
			if _, loaded := ids.LoadOrStore(op.ID, true); loaded {
				t.Errorf("duplicate ID: %s", op.ID)
			}
		}()
	}
	wg.Wait()
}

func TestNew_InitialState(t *testing.T) {
	op := New(TypeCommand, "parent_1")
	if op.Status != StatusRunning {
		t.Errorf("expected StatusRunning, got %v", op.Status)
	}
	if op.ParentID != "parent_1" {
		t.Errorf("expected parent_1, got %q", op.ParentID)
	}
	if op.StartTime.IsZero() {
		t.Error("expected non-zero StartTime")
	}
	if !op.EndTime.IsZero() {
		t.Error("expected zero EndTime")
	}
	if len(op.Context) != 0 {
		t.Error("expected empty context")
	}
}

func TestEnrich_And_Get(t *testing.T) {
	op := New(TypeCommand, "")
	op.Enrich("_start_ns", "12345")
	op.Enrich("shared.username", "john")

	v, ok := op.Get("_start_ns")
	if !ok || v != "12345" {
		t.Errorf("expected 12345, got %q", v)
	}

	v, ok = op.Get("shared.username")
	if !ok || v != "john" {
		t.Errorf("expected john, got %q", v)
	}

	_, ok = op.Get("nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestEnrichMany(t *testing.T) {
	op := New(TypeCommand, "")
	op.EnrichMany(map[string]string{
		"_start_ns":       "12345",
		"shared.username": "john",
	})

	v, _ := op.Get("_start_ns")
	if v != "12345" {
		t.Errorf("expected 12345, got %q", v)
	}

	// nil/empty is a no-op
	op.EnrichMany(nil)
	op.EnrichMany(map[string]string{})
}

func TestConcurrentEnrich(t *testing.T) {
	op := New(TypeCommand, "")
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			op.Enrich(strings.Repeat("k", n%10+1), "v")
			op.Get(strings.Repeat("k", n%10+1))
		}(i)
	}
	wg.Wait()
}

func TestContextSnapshot(t *testing.T) {
	op := New(TypeCommand, "")
	op.Enrich("_start_ns", "123")
	op.Enrich("shared.username", "john")
	op.Enrich("shared.secret.jwt", "eyJ...")
	op.Enrich("_secret.session", "tok")

	t.Run("not redacted", func(t *testing.T) {
		snap := op.ContextSnapshot(false)
		if snap["shared.secret.jwt"] != "eyJ..." {
			t.Error("expected secrets in unredacted snapshot")
		}
		if snap["_secret.session"] != "tok" {
			t.Error("expected server secrets in unredacted snapshot")
		}
	})

	t.Run("redacted", func(t *testing.T) {
		snap := op.ContextSnapshot(true)
		if _, ok := snap["shared.secret.jwt"]; ok {
			t.Error("secrets should be redacted")
		}
		if _, ok := snap["_secret.session"]; ok {
			t.Error("server secrets should be redacted")
		}
		if snap["_start_ns"] != "123" {
			t.Error("non-secret keys should survive redaction")
		}
		if snap["shared.username"] != "john" {
			t.Error("non-secret shared keys should survive redaction")
		}
	})

	t.Run("snapshot is a copy", func(t *testing.T) {
		snap := op.ContextSnapshot(false)
		snap["mutated"] = "yes"
		_, ok := op.Get("mutated")
		if ok {
			t.Error("snapshot mutation should not affect operation")
		}
	})
}

func TestFilteredContext(t *testing.T) {
	op := New(TypeCommand, "")
	op.Enrich("_start_ns", "123")
	op.Enrich("auth.cache_hit", "true")
	op.Enrich("auth.secret.api_key", "key123")
	op.Enrich("gobservability.span_id", "span1")
	op.Enrich("shared.username", "john")
	op.Enrich("shared.secret.jwt", "eyJ...")

	t.Run("auth sees own + server + shared", func(t *testing.T) {
		filtered := op.FilteredContext("auth", false)
		if filtered["auth.cache_hit"] != "true" {
			t.Error("auth should see own keys")
		}
		if filtered["_start_ns"] != "123" {
			t.Error("auth should see server keys")
		}
		if filtered["shared.username"] != "john" {
			t.Error("auth should see shared keys")
		}
		if _, ok := filtered["gobservability.span_id"]; ok {
			t.Error("auth should NOT see gobservability keys")
		}
	})

	t.Run("auth filtered + redacted", func(t *testing.T) {
		filtered := op.FilteredContext("auth", true)
		if _, ok := filtered["auth.secret.api_key"]; ok {
			t.Error("secrets should be redacted")
		}
		if _, ok := filtered["shared.secret.jwt"]; ok {
			t.Error("shared secrets should be redacted")
		}
		if filtered["auth.cache_hit"] != "true" {
			t.Error("non-secret keys should survive")
		}
	})
}

func TestComplete(t *testing.T) {
	op := New(TypeCommand, "")
	time.Sleep(time.Millisecond) // ensure measurable duration
	op.Complete()

	if op.Status != StatusCompleted {
		t.Errorf("expected StatusCompleted, got %v", op.Status)
	}
	if op.EndTime.IsZero() {
		t.Error("expected non-zero EndTime")
	}
	if op.Duration() <= 0 {
		t.Error("expected positive duration")
	}
}

func TestFail(t *testing.T) {
	op := New(TypeCommand, "")
	op.Fail("out of memory")

	if op.Status != StatusFailed {
		t.Errorf("expected StatusFailed, got %v", op.Status)
	}
	if op.FailReason != "out of memory" {
		t.Errorf("expected fail reason, got %q", op.FailReason)
	}
	if op.EndTime.IsZero() {
		t.Error("expected non-zero EndTime")
	}
}

func TestDuration_Running(t *testing.T) {
	op := New(TypeCommand, "")
	time.Sleep(5 * time.Millisecond)
	d := op.Duration()
	if d < 5*time.Millisecond {
		t.Errorf("expected at least 5ms, got %v", d)
	}
}

func TestDuration_Completed(t *testing.T) {
	op := New(TypeCommand, "")
	time.Sleep(5 * time.Millisecond)
	op.Complete()
	d1 := op.Duration()
	time.Sleep(10 * time.Millisecond)
	d2 := op.Duration()
	// After completion, duration should be fixed.
	if d1 != d2 {
		t.Errorf("completed duration should be fixed: %v vs %v", d1, d2)
	}
}

func TestStatus_String(t *testing.T) {
	if StatusRunning.String() != "running" {
		t.Error("unexpected string for Running")
	}
	if StatusCompleted.String() != "completed" {
		t.Error("unexpected string for Completed")
	}
	if StatusFailed.String() != "failed" {
		t.Error("unexpected string for Failed")
	}
}
