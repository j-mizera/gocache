package handler_test

import (
	"errors"
	"strings"
	"testing"

	apicommand "gocache/api/command"
	"gocache/pkg/cache"
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
	keys := res.Value.([]any)
	if len(keys) != 3 {
		t.Errorf("Expected 3 keys, got %d", len(keys))
	}

	// Test HVALS
	res = eval(t, c, e, ctx, "HVALS", []string{"user:1"})
	if res.Err != nil {
		t.Fatalf("HVALS failed: %v", res.Err)
	}
	vals := res.Value.([]any)
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
	if !errors.Is(res.Err, apicommand.ErrWrongType) {
		t.Error("Expected WRONGTYPE error for HGET on string key")
	}
}

// TestHash_EncodingPromotion verifies a hash starts as EncPacked when
// small and transparently promotes to EncNative once the count crosses
// the configured threshold. Values and semantics stay identical.
func TestHash_EncodingPromotion(t *testing.T) {
	c, e, ctx := setup(t)
	// Tight thresholds: 3 entries.
	c.SetPackedThresholds(cache.PackedThresholds{HashMaxEntries: 3, HashMaxValue: 1024})

	for i := 0; i < 3; i++ {
		res := eval(t, c, e, ctx, "HSET", []string{"h", "f" + string(rune('0'+i)), "v"})
		if res.Err != nil {
			t.Fatalf("HSET %d: %v", i, res.Err)
		}
	}
	entry, ok := c.RawGet("h")
	if !ok {
		t.Fatal("h not set")
	}
	if entry.Encoding != cache.EncPacked {
		t.Errorf("after 3 inserts: Encoding = %v; want EncPacked", entry.Encoding)
	}

	// Fourth insert crosses threshold — promotion should happen.
	res := eval(t, c, e, ctx, "HSET", []string{"h", "f3", "v"})
	if res.Err != nil {
		t.Fatalf("HSET 4: %v", res.Err)
	}
	entry, _ = c.RawGet("h")
	if entry.Encoding != cache.EncNative {
		t.Errorf("after 4 inserts: Encoding = %v; want EncNative", entry.Encoding)
	}

	// HGETALL still returns all four.
	res = eval(t, c, e, ctx, "HGETALL", []string{"h"})
	m := res.Value.(map[string]string)
	if len(m) != 4 {
		t.Errorf("HGETALL after promotion: len = %d; want 4", len(m))
	}
}

// TestHash_PromotionOnValueLength exercises the per-value length threshold.
func TestHash_PromotionOnValueLength(t *testing.T) {
	c, e, ctx := setup(t)
	c.SetPackedThresholds(cache.PackedThresholds{HashMaxEntries: 512, HashMaxValue: 5})

	eval(t, c, e, ctx, "HSET", []string{"h", "k", "short"})
	entry, _ := c.RawGet("h")
	if entry.Encoding != cache.EncPacked {
		t.Errorf("short value: Encoding = %v; want EncPacked", entry.Encoding)
	}

	eval(t, c, e, ctx, "HSET", []string{"h", "k", strings.Repeat("x", 6)})
	entry, _ = c.RawGet("h")
	if entry.Encoding != cache.EncNative {
		t.Errorf("after long value: Encoding = %v; want EncNative", entry.Encoding)
	}
	res := eval(t, c, e, ctx, "HGET", []string{"h", "k"})
	if res.Value != strings.Repeat("x", 6) {
		t.Errorf("HGET after promotion: %v", res.Value)
	}
}
