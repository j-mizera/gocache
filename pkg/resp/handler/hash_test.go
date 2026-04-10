package handler_test

import (
	"errors"
	"gocache/pkg/resp"
	"testing"
)

func TestEvaluator_Hash(t *testing.T) {
	c, e, ctx := setup(t)

	// Test HSET single field
	res := eval(t, c, e, ctx, "HSET", []string{"user:1", "name", "Alice"})
	if res.Err != nil {
		t.Fatalf("HSET failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 new field, got %v", res.Value)
	}

	// Test HGET
	res = eval(t, c, e, ctx, "HGET", []string{"user:1", "name"})
	if res.Err != nil {
		t.Fatalf("HGET failed: %v", res.Err)
	}
	if res.Value != "Alice" {
		t.Errorf("Expected 'Alice', got %v", res.Value)
	}

	// Test HSET multiple fields
	res = eval(t, c, e, ctx, "HSET", []string{"user:1", "age", "30", "city", "NYC"})
	if res.Err != nil {
		t.Fatalf("HSET multiple failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 new fields, got %v", res.Value)
	}

	// Test HEXISTS
	res = eval(t, c, e, ctx, "HEXISTS", []string{"user:1", "age"})
	if res.Err != nil {
		t.Fatalf("HEXISTS failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 (exists), got %v", res.Value)
	}

	res = eval(t, c, e, ctx, "HEXISTS", []string{"user:1", "nonexistent"})
	if res.Err != nil {
		t.Fatalf("HEXISTS failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected 0 (not exists), got %v", res.Value)
	}

	// Test HLEN
	res = eval(t, c, e, ctx, "HLEN", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HLEN failed: %v", res.Err)
	}
	if res.Value != 3 {
		t.Errorf("Expected 3 fields, got %v", res.Value)
	}

	// Test HKEYS
	res = eval(t, c, e, ctx, "HKEYS", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HKEYS failed: %v", res.Err)
	}
	keys := res.Value.([]interface{})
	if len(keys) != 3 {
		t.Errorf("Expected 3 keys, got %d", len(keys))
	}

	// Test HVALS
	res = eval(t, c, e, ctx, "HVALS", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HVALS failed: %v", res.Err)
	}
	vals := res.Value.([]interface{})
	if len(vals) != 3 {
		t.Errorf("Expected 3 values, got %d", len(vals))
	}

	// Test HGETALL
	res = eval(t, c, e, ctx, "HGETALL", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HGETALL failed: %v", res.Err)
	}
	all, ok := res.Value.(map[string]string)
	if !ok {
		t.Fatalf("Expected map[string]string, got %T", res.Value)
	}
	if len(all) != 3 {
		t.Errorf("Expected 3 field-value pairs, got %d", len(all))
	}

	// Test HDEL
	res = eval(t, c, e, ctx, "HDEL", []string{"user:1", "age"})
	if res.Err != nil {
		t.Fatalf("HDEL failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 deleted, got %v", res.Value)
	}

	// Verify deletion
	res = eval(t, c, e, ctx, "HLEN", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HLEN after HDEL failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 fields after deletion, got %v", res.Value)
	}

	// Test HDEL all fields
	res = eval(t, c, e, ctx, "HDEL", []string{"user:1", "name", "city"})
	if res.Err != nil {
		t.Fatalf("HDEL all failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 deleted, got %v", res.Value)
	}

	// Verify key is deleted when hash is empty
	res = eval(t, c, e, ctx, "EXISTS", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("EXISTS failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected key to be deleted when hash is empty, got %v", res.Value)
	}

	// Test WRONGTYPE error
	eval(t, c, e, ctx, "SET", []string{"stringkey", "value"})
	res = eval(t, c, e, ctx, "HGET", []string{"stringkey", "field"})
	if !errors.Is(res.Err, resp.ErrWrongType) {
		t.Error("Expected WRONGTYPE error for HGET on string key")
	}
}
