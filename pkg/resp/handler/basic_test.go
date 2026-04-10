package handler_test

import (
	"gocache/pkg/resp"
	"strings"
	"testing"
)

func TestEvaluator_Basic(t *testing.T) {
	c, e, ctx := setup(t)

	// Test SET
	res := eval(t, c, e, ctx, "SET", []string{"key", "value"})
	if res.Err != nil {
		t.Errorf("expected no error, got %v", res.Err)
	}
	if res.Value != "OK" {
		t.Errorf("expected OK, got %v", res.Value)
	}

	// Test GET
	res = eval(t, c, e, ctx, "GET", []string{"key"})
	if res.Err != nil {
		t.Errorf("expected no error, got %v", res.Err)
	}
	if res.Value != "value" {
		t.Errorf("expected value, got %v", res.Value)
	}

	// Test EXISTS
	res = eval(t, c, e, ctx, "EXISTS", []string{"key"})
	if res.Err != nil {
		t.Errorf("expected no error, got %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("expected 1, got %v", res.Value)
	}

	// Test DEL
	res = eval(t, c, e, ctx, "DEL", []string{"key"})
	if res.Err != nil {
		t.Errorf("expected no error, got %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("expected 1, got %v", res.Value)
	}

	// Test EXISTS after DEL
	res = eval(t, c, e, ctx, "EXISTS", []string{"key"})
	if res.Err != nil {
		t.Errorf("expected no error, got %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("expected 0, got %v", res.Value)
	}
}

func TestEvaluator_TTL(t *testing.T) {
	c, e, ctx := setup(t)

	eval(t, c, e, ctx, "SET", []string{"temp", "val", "PX", "100"})

	res := eval(t, c, e, ctx, "TTL", []string{"temp"})
	if res.Value.(int64) < 0 {
		t.Errorf("expected positive TTL, got %v", res.Value)
	}

	// Test EXPIRE (integer seconds)
	eval(t, c, e, ctx, "SET", []string{"expireme", "val"})
	res = eval(t, c, e, ctx, "EXPIRE", []string{"expireme", "1"})
	if res.Value != 1 {
		t.Errorf("expected 1 (success), got %v", res.Value)
	}
}

func TestEvaluator_DBSize(t *testing.T) {
	c, e, ctx := setup(t)

	// Empty cache
	res := eval(t, c, e, ctx, "DBSIZE", nil)
	if res.Err != nil {
		t.Fatalf("DBSIZE failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("expected 0, got %v", res.Value)
	}

	// Add some keys
	eval(t, c, e, ctx, "SET", []string{"a", "1"})
	eval(t, c, e, ctx, "SET", []string{"b", "2"})
	eval(t, c, e, ctx, "SET", []string{"c", "3"})

	res = eval(t, c, e, ctx, "DBSIZE", nil)
	if res.Err != nil {
		t.Fatalf("DBSIZE failed: %v", res.Err)
	}
	if res.Value != 3 {
		t.Errorf("expected 3, got %v", res.Value)
	}

	// Delete one
	eval(t, c, e, ctx, "DEL", []string{"b"})

	res = eval(t, c, e, ctx, "DBSIZE", nil)
	if res.Err != nil {
		t.Fatalf("DBSIZE failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("expected 2, got %v", res.Value)
	}
}

func TestEvaluator_Info(t *testing.T) {
	c, e, ctx := setup(t)

	eval(t, c, e, ctx, "SET", []string{"x", "hello"})

	res := eval(t, c, e, ctx, "INFO", []string{"memory"})
	if res.Err != nil {
		t.Fatalf("INFO memory failed: %v", res.Err)
	}

	info, ok := res.Value.(string)
	if !ok {
		t.Fatalf("expected string, got %T", res.Value)
	}

	for _, field := range []string{"used_memory:", "maxmemory:", "maxmemory_policy:", "keys:1", "eviction_policy:"} {
		if !strings.Contains(info, field) {
			t.Errorf("INFO output missing %q:\n%s", field, info)
		}
	}

	// INFO with no args should also work
	res = eval(t, c, e, ctx, "INFO", nil)
	if res.Err != nil {
		t.Fatalf("INFO (no args) failed: %v", res.Err)
	}
	if _, ok := res.Value.(string); !ok {
		t.Errorf("expected string, got %T", res.Value)
	}

	// INFO with unknown section returns empty string
	res = eval(t, c, e, ctx, "INFO", []string{"replication"})
	if res.Err != nil {
		t.Fatalf("INFO replication failed: %v", res.Err)
	}
	if res.Value != "" {
		t.Errorf("expected empty string for unknown section, got %v", res.Value)
	}
}

func TestEvaluator_Hello(t *testing.T) {
	c, e, ctx := setup(t)

	t.Run("HELLO 2 keeps proto at 2", func(t *testing.T) {
		res := eval(t, c, e, ctx, "HELLO", []string{"2"})
		if res.Err != nil {
			t.Fatalf("HELLO 2 failed: %v", res.Err)
		}
		info, ok := res.Value.(map[string]interface{})
		if !ok {
			t.Fatalf("expected map[string]interface{}, got %T", res.Value)
		}
		if info["proto"] != 2 {
			t.Errorf("expected proto=2, got %v", info["proto"])
		}
		if ctx.ProtoVersion != 2 {
			t.Errorf("expected ProtoVersion=2, got %d", ctx.ProtoVersion)
		}
	})

	t.Run("HELLO 3 upgrades to RESP3", func(t *testing.T) {
		res := eval(t, c, e, ctx, "HELLO", []string{"3"})
		if res.Err != nil {
			t.Fatalf("HELLO 3 failed: %v", res.Err)
		}
		info, ok := res.Value.(map[string]interface{})
		if !ok {
			t.Fatalf("expected map[string]interface{}, got %T", res.Value)
		}
		if info["proto"] != 3 {
			t.Errorf("expected proto=3, got %v", info["proto"])
		}
		if ctx.ProtoVersion != 3 {
			t.Errorf("expected ProtoVersion=3, got %d", ctx.ProtoVersion)
		}
	})

	t.Run("HELLO 4 returns NOPROTO", func(t *testing.T) {
		res := eval(t, c, e, ctx, "HELLO", []string{"4"})
		v, ok := res.Value.(resp.Value)
		if !ok || v.Type != resp.Error {
			t.Fatalf("expected NOPROTO error, got %T: %v", res.Value, res.Value)
		}
		if !strings.Contains(v.Str, "NOPROTO") {
			t.Errorf("expected NOPROTO in error, got %q", v.Str)
		}
	})
}
