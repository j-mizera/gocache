package handler_test

import (
	"errors"
	"strings"
	"testing"

	apicommand "gocache/api/command"
	"gocache/pkg/cache"
)

func TestEvaluator_Set(t *testing.T) {
	c, e, ctx := setup(t)

	// Test SADD single member
	res := eval(t, c, e, ctx, "SADD", []string{"myset", "apple"})
	if res.Err != nil {
		t.Fatalf("SADD failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 added, got %v", res.Value)
	}

	// Test SADD multiple members
	res = eval(t, c, e, ctx, "SADD", []string{"myset", "banana", "cherry", "apple"})
	if res.Err != nil {
		t.Fatalf("SADD multiple failed: %v", res.Err)
	}
	if res.Value != 2 { // apple already exists
		t.Errorf("Expected 2 added, got %v", res.Value)
	}

	// Test SISMEMBER
	res = eval(t, c, e, ctx, "SISMEMBER", []string{"myset", "banana"})
	if res.Err != nil {
		t.Fatalf("SISMEMBER failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 (member exists), got %v", res.Value)
	}

	res = eval(t, c, e, ctx, "SISMEMBER", []string{"myset", "orange"})
	if res.Err != nil {
		t.Fatalf("SISMEMBER failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected 0 (member not exists), got %v", res.Value)
	}

	// Test SCARD
	res = eval(t, c, e, ctx, "SCARD", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SCARD failed: %v", res.Err)
	}
	if res.Value != 3 {
		t.Errorf("Expected 3 members, got %v", res.Value)
	}

	// Test SMEMBERS
	res = eval(t, c, e, ctx, "SMEMBERS", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SMEMBERS failed: %v", res.Err)
	}
	members := res.Value.([]any)
	if len(members) != 3 {
		t.Errorf("Expected 3 members, got %d", len(members))
	}

	// Test SREM
	res = eval(t, c, e, ctx, "SREM", []string{"myset", "banana"})
	if res.Err != nil {
		t.Fatalf("SREM failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 removed, got %v", res.Value)
	}

	// Verify removal
	res = eval(t, c, e, ctx, "SCARD", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SCARD after SREM failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 members after removal, got %v", res.Value)
	}

	// Test SPOP
	res = eval(t, c, e, ctx, "SPOP", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SPOP failed: %v", res.Err)
	}
	if res.Value == nil {
		t.Error("Expected a member, got nil")
	}

	// Verify size after pop
	res = eval(t, c, e, ctx, "SCARD", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SCARD after SPOP failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 member after pop, got %v", res.Value)
	}

	// Test removing all members
	res = eval(t, c, e, ctx, "SPOP", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SPOP failed: %v", res.Err)
	}

	// Verify key is deleted when set is empty
	res = eval(t, c, e, ctx, "EXISTS", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("EXISTS failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected key to be deleted when set is empty, got %v", res.Value)
	}

	// Test WRONGTYPE error
	eval(t, c, e, ctx, "SET", []string{"stringkey", "value"})
	res = eval(t, c, e, ctx, "SADD", []string{"stringkey", "member"})
	if !errors.Is(res.Err, apicommand.ErrWrongType) {
		t.Error("Expected WRONGTYPE error for SADD on string key")
	}
}

// TestSet_EncodingPromotion verifies a set starts as EncPacked when small
// and promotes to EncNative once the entry count crosses SetMaxEntries.
func TestSet_EncodingPromotion(t *testing.T) {
	c, e, ctx := setup(t)
	c.SetPackedThresholds(cache.PackedThresholds{SetMaxEntries: 3, SetMaxValue: 1024})

	for i := 0; i < 3; i++ {
		eval(t, c, e, ctx, "SADD", []string{"s", "m" + string(rune('0'+i))})
	}
	entry, _ := c.RawGet("s")
	if entry.Encoding != cache.EncPacked {
		t.Errorf("after 3 adds: Encoding = %v; want EncPacked", entry.Encoding)
	}

	eval(t, c, e, ctx, "SADD", []string{"s", "m3"})
	entry, _ = c.RawGet("s")
	if entry.Encoding != cache.EncNative {
		t.Errorf("after 4 adds: Encoding = %v; want EncNative", entry.Encoding)
	}

	// Semantics still intact after promotion.
	res := eval(t, c, e, ctx, "SCARD", []string{"s"})
	if res.Value != 4 {
		t.Errorf("SCARD = %v; want 4", res.Value)
	}
}

// TestSet_PromotionOnMemberLength exercises the per-member length threshold.
func TestSet_PromotionOnMemberLength(t *testing.T) {
	c, e, ctx := setup(t)
	c.SetPackedThresholds(cache.PackedThresholds{SetMaxEntries: 512, SetMaxValue: 5})

	eval(t, c, e, ctx, "SADD", []string{"s", "short"})
	entry, _ := c.RawGet("s")
	if entry.Encoding != cache.EncPacked {
		t.Errorf("short member: Encoding = %v; want EncPacked", entry.Encoding)
	}
	eval(t, c, e, ctx, "SADD", []string{"s", strings.Repeat("x", 6)})
	entry, _ = c.RawGet("s")
	if entry.Encoding != cache.EncNative {
		t.Errorf("after long member: Encoding = %v; want EncNative", entry.Encoding)
	}
}
