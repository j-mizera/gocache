package evaluator

import (
	"errors"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/resp"
	"testing"
)

func TestEvaluator_Set(t *testing.T) {
	cacheInstance := cache.New()
	engineInstance := engine.New(cacheInstance)
	go engineInstance.Run()
	defer engineInstance.Stop()

	evalInstance := New(cacheInstance, engineInstance, "", "", nil, nil)
	ctx := clientctx.New()

	// Test SADD single member
	res := evalInstance.Evaluate(ctx, "SADD", []string{"myset", "apple"})
	if res.Err != nil {
		t.Fatalf("SADD failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 added, got %v", res.Value)
	}

	// Test SADD multiple members
	res = evalInstance.Evaluate(ctx, "SADD", []string{"myset", "banana", "cherry", "apple"})
	if res.Err != nil {
		t.Fatalf("SADD multiple failed: %v", res.Err)
	}
	if res.Value != 2 { // apple already exists
		t.Errorf("Expected 2 added, got %v", res.Value)
	}

	// Test SISMEMBER
	res = evalInstance.Evaluate(ctx, "SISMEMBER", []string{"myset", "banana"})
	if res.Err != nil {
		t.Fatalf("SISMEMBER failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 (member exists), got %v", res.Value)
	}

	res = evalInstance.Evaluate(ctx, "SISMEMBER", []string{"myset", "orange"})
	if res.Err != nil {
		t.Fatalf("SISMEMBER failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected 0 (member not exists), got %v", res.Value)
	}

	// Test SCARD
	res = evalInstance.Evaluate(ctx, "SCARD", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SCARD failed: %v", res.Err)
	}
	if res.Value != 3 {
		t.Errorf("Expected 3 members, got %v", res.Value)
	}

	// Test SMEMBERS
	res = evalInstance.Evaluate(ctx, "SMEMBERS", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SMEMBERS failed: %v", res.Err)
	}
	members := res.Value.([]interface{})
	if len(members) != 3 {
		t.Errorf("Expected 3 members, got %d", len(members))
	}

	// Test SREM
	res = evalInstance.Evaluate(ctx, "SREM", []string{"myset", "banana"})
	if res.Err != nil {
		t.Fatalf("SREM failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 removed, got %v", res.Value)
	}

	// Verify removal
	res = evalInstance.Evaluate(ctx, "SCARD", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SCARD after SREM failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 members after removal, got %v", res.Value)
	}

	// Test SPOP
	res = evalInstance.Evaluate(ctx, "SPOP", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SPOP failed: %v", res.Err)
	}
	if res.Value == nil {
		t.Error("Expected a member, got nil")
	}

	// Verify size after pop
	res = evalInstance.Evaluate(ctx, "SCARD", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SCARD after SPOP failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 member after pop, got %v", res.Value)
	}

	// Test removing all members
	res = evalInstance.Evaluate(ctx, "SPOP", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("SPOP failed: %v", res.Err)
	}

	// Verify key is deleted when set is empty
	res = evalInstance.Evaluate(ctx, "EXISTS", []string{"myset"})
	if res.Err != nil {
		t.Fatalf("EXISTS failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected key to be deleted when set is empty, got %v", res.Value)
	}

	// Test WRONGTYPE error
	evalInstance.Evaluate(ctx, "SET", []string{"stringkey", "value"})
	res = evalInstance.Evaluate(ctx, "SADD", []string{"stringkey", "member"})
	if !errors.Is(res.Err, resp.ErrWrongType) {
		t.Error("Expected WRONGTYPE error for SADD on string key")
	}
}
