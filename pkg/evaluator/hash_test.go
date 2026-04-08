package evaluator

import (
	"errors"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/resp"
	"testing"
)

func TestEvaluator_Hash(t *testing.T) {
	cacheInstance := cache.New()
	engineInstance := engine.New(cacheInstance)
	go engineInstance.Run()
	defer engineInstance.Stop()

	evalInstance := New(cacheInstance, engineInstance, "", "", nil, nil)
	ctx := clientctx.New()

	// Test HSET single field
	res := evalInstance.Evaluate(ctx, "HSET", []string{"user:1", "name", "Alice"})
	if res.Err != nil {
		t.Fatalf("HSET failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 new field, got %v", res.Value)
	}

	// Test HGET
	res = evalInstance.Evaluate(ctx, "HGET", []string{"user:1", "name"})
	if res.Err != nil {
		t.Fatalf("HGET failed: %v", res.Err)
	}
	if res.Value != "Alice" {
		t.Errorf("Expected 'Alice', got %v", res.Value)
	}

	// Test HSET multiple fields
	res = evalInstance.Evaluate(ctx, "HSET", []string{"user:1", "age", "30", "city", "NYC"})
	if res.Err != nil {
		t.Fatalf("HSET multiple failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 new fields, got %v", res.Value)
	}

	// Test HEXISTS
	res = evalInstance.Evaluate(ctx, "HEXISTS", []string{"user:1", "age"})
	if res.Err != nil {
		t.Fatalf("HEXISTS failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 (exists), got %v", res.Value)
	}

	res = evalInstance.Evaluate(ctx, "HEXISTS", []string{"user:1", "nonexistent"})
	if res.Err != nil {
		t.Fatalf("HEXISTS failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected 0 (not exists), got %v", res.Value)
	}

	// Test HLEN
	res = evalInstance.Evaluate(ctx, "HLEN", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HLEN failed: %v", res.Err)
	}
	if res.Value != 3 {
		t.Errorf("Expected 3 fields, got %v", res.Value)
	}

	// Test HKEYS
	res = evalInstance.Evaluate(ctx, "HKEYS", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HKEYS failed: %v", res.Err)
	}
	keys := res.Value.([]interface{})
	if len(keys) != 3 {
		t.Errorf("Expected 3 keys, got %d", len(keys))
	}

	// Test HVALS
	res = evalInstance.Evaluate(ctx, "HVALS", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HVALS failed: %v", res.Err)
	}
	vals := res.Value.([]interface{})
	if len(vals) != 3 {
		t.Errorf("Expected 3 values, got %d", len(vals))
	}

	// Test HGETALL
	res = evalInstance.Evaluate(ctx, "HGETALL", []string{"user:1"})
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
	res = evalInstance.Evaluate(ctx, "HDEL", []string{"user:1", "age"})
	if res.Err != nil {
		t.Fatalf("HDEL failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 deleted, got %v", res.Value)
	}

	// Verify deletion
	res = evalInstance.Evaluate(ctx, "HLEN", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HLEN after HDEL failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 fields after deletion, got %v", res.Value)
	}

	// Test HDEL all fields
	res = evalInstance.Evaluate(ctx, "HDEL", []string{"user:1", "name", "city"})
	if res.Err != nil {
		t.Fatalf("HDEL all failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 deleted, got %v", res.Value)
	}

	// Verify key is deleted when hash is empty
	res = evalInstance.Evaluate(ctx, "EXISTS", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("EXISTS failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected key to be deleted when hash is empty, got %v", res.Value)
	}

	// Test WRONGTYPE error
	evalInstance.Evaluate(ctx, "SET", []string{"stringkey", "value"})
	res = evalInstance.Evaluate(ctx, "HGET", []string{"stringkey", "field"})
	if !errors.Is(res.Err, resp.ErrWrongType) {
		t.Error("Expected WRONGTYPE error for HGET on string key")
	}
}
