package evaluator

import (
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"testing"
)

func TestEvaluator_SortedSet(t *testing.T) {
	cacheInstance := cache.New()
	engineInstance := engine.New(cacheInstance)
	go engineInstance.Run()
	defer engineInstance.Stop()

	evalInstance := New(cacheInstance, engineInstance, "", "", nil, nil)
	ctx := clientctx.New()

	// Test ZADD single member
	res := evalInstance.Evaluate(ctx, "ZADD", []string{"leaderboard", "100", "player1"})
	if res.Err != nil {
		t.Fatalf("ZADD failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 added, got %v", res.Value)
	}

	// Test ZADD multiple members
	res = evalInstance.Evaluate(ctx, "ZADD", []string{"leaderboard", "200", "player2", "150", "player3", "100", "player1"})
	if res.Err != nil {
		t.Fatalf("ZADD multiple failed: %v", res.Err)
	}
	if res.Value != 2 { // player1 already exists (updated score)
		t.Errorf("Expected 2 added, got %v", res.Value)
	}

	// Test ZCARD
	res = evalInstance.Evaluate(ctx, "ZCARD", []string{"leaderboard"})
	if res.Err != nil {
		t.Fatalf("ZCARD failed: %v", res.Err)
	}
	if res.Value != 3 {
		t.Errorf("Expected 3 members, got %v", res.Value)
	}

	// Test ZSCORE
	res = evalInstance.Evaluate(ctx, "ZSCORE", []string{"leaderboard", "player2"})
	if res.Err != nil {
		t.Fatalf("ZSCORE failed: %v", res.Err)
	}
	if res.Value != "200" {
		t.Errorf("Expected score '200', got %v", res.Value)
	}

	// Test ZSCORE for non-existent member
	res = evalInstance.Evaluate(ctx, "ZSCORE", []string{"leaderboard", "player99"})
	if res.Err != nil {
		t.Fatalf("ZSCORE failed: %v", res.Err)
	}
	if res.Value != nil {
		t.Errorf("Expected nil for non-existent member, got %v", res.Value)
	}

	// Test ZRANGE without scores
	res = evalInstance.Evaluate(ctx, "ZRANGE", []string{"leaderboard", "0", "-1"})
	if res.Err != nil {
		t.Fatalf("ZRANGE failed: %v", res.Err)
	}
	members := res.Value.([]interface{})
	if len(members) != 3 {
		t.Errorf("Expected 3 members, got %d", len(members))
	}
	// Should be sorted by score: player1 (100), player3 (150), player2 (200)
	if members[0] != "player1" {
		t.Errorf("Expected player1 first, got %v", members[0])
	}
	if members[1] != "player3" {
		t.Errorf("Expected player3 second, got %v", members[1])
	}
	if members[2] != "player2" {
		t.Errorf("Expected player2 third, got %v", members[2])
	}

	// Test ZRANGE with scores
	res = evalInstance.Evaluate(ctx, "ZRANGE", []string{"leaderboard", "0", "1", "WITHSCORES"})
	if res.Err != nil {
		t.Fatalf("ZRANGE WITHSCORES failed: %v", res.Err)
	}
	withScores := res.Value.([]interface{})
	if len(withScores) != 4 { // 2 members * 2 (member + score)
		t.Errorf("Expected 4 items, got %d", len(withScores))
	}
	if withScores[0] != "player1" || withScores[1].(float64) != 100 {
		t.Errorf("Unexpected first member/score: %v %v", withScores[0], withScores[1])
	}

	// Test ZRANGE with negative indices
	res = evalInstance.Evaluate(ctx, "ZRANGE", []string{"leaderboard", "-2", "-1"})
	if res.Err != nil {
		t.Fatalf("ZRANGE with negative indices failed: %v", res.Err)
	}
	negMembers := res.Value.([]interface{})
	if len(negMembers) != 2 {
		t.Errorf("Expected 2 members, got %d", len(negMembers))
	}
	if negMembers[0] != "player3" || negMembers[1] != "player2" {
		t.Errorf("Unexpected members: %v %v", negMembers[0], negMembers[1])
	}

	// Test ZRANK
	res = evalInstance.Evaluate(ctx, "ZRANK", []string{"leaderboard", "player3"})
	if res.Err != nil {
		t.Fatalf("ZRANK failed: %v", res.Err)
	}
	if rank, ok := res.Value.(int); !ok || rank != 1 {
		t.Errorf("Expected rank 1, got %v", res.Value)
	}

	// Test ZRANK for non-existent member
	res = evalInstance.Evaluate(ctx, "ZRANK", []string{"leaderboard", "player99"})
	if res.Err != nil {
		t.Fatalf("ZRANK failed: %v", res.Err)
	}
	if res.Value != nil {
		t.Errorf("Expected nil for non-existent member, got %v", res.Value)
	}

	// Test ZCOUNT
	res = evalInstance.Evaluate(ctx, "ZCOUNT", []string{"leaderboard", "100", "180"})
	if res.Err != nil {
		t.Fatalf("ZCOUNT failed: %v", res.Err)
	}
	if count, ok := res.Value.(int); !ok || count != 2 {
		t.Errorf("Expected count 2, got %v", res.Value)
	}

	// Test ZREM
	res = evalInstance.Evaluate(ctx, "ZREM", []string{"leaderboard", "player3"})
	if res.Err != nil {
		t.Fatalf("ZREM failed: %v", res.Err)
	}
	if res.Value != 1 {
		t.Errorf("Expected 1 removed, got %v", res.Value)
	}

	// Verify removal
	res = evalInstance.Evaluate(ctx, "ZCARD", []string{"leaderboard"})
	if res.Err != nil {
		t.Fatalf("ZCARD after ZREM failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 members after removal, got %v", res.Value)
	}

	// Test ZREM multiple members
	res = evalInstance.Evaluate(ctx, "ZREM", []string{"leaderboard", "player1", "player2"})
	if res.Err != nil {
		t.Fatalf("ZREM multiple failed: %v", res.Err)
	}
	if res.Value != 2 {
		t.Errorf("Expected 2 removed, got %v", res.Value)
	}

	// Verify key is deleted when sorted set is empty
	res = evalInstance.Evaluate(ctx, "EXISTS", []string{"leaderboard"})
	if res.Err != nil {
		t.Fatalf("EXISTS failed: %v", res.Err)
	}
	if res.Value != 0 {
		t.Errorf("Expected key to be deleted when sorted set is empty, got %v", res.Value)
	}

	// Test WRONGTYPE error
	evalInstance.Evaluate(ctx, "SET", []string{"stringkey", "value"})
	res = evalInstance.Evaluate(ctx, "ZADD", []string{"stringkey", "100", "member"})
	if res.Err == nil {
		t.Error("Expected WRONGTYPE error for ZADD on string key")
	}

	// Test invalid score
	res = evalInstance.Evaluate(ctx, "ZADD", []string{"newzset", "notanumber", "member"})
	if res.Err == nil {
		t.Error("Expected error for invalid score")
	}
}

func TestZadd_OOM_NoEviction(t *testing.T) {
	// Use a tiny cache with noeviction — ZADD should return an OOM error.
	cacheInstance := cache.NewWithBytes(1, cache.EvictionNone)
	engineInstance := engine.New(cacheInstance)
	go engineInstance.Run()
	defer engineInstance.Stop()

	evalInstance := New(cacheInstance, engineInstance, "", "", nil, nil)
	ctx := clientctx.New()

	// First ZADD may succeed if entry fits; subsequent must fail.
	_ = evalInstance.Evaluate(ctx, "ZADD", []string{"zz", "1", "m1"})

	res := evalInstance.Evaluate(ctx, "ZADD", []string{"zz2", "2", "m2"})
	if res.Err == nil {
		t.Error("expected OOM error from ZADD with noeviction, got nil")
	}
}

func TestSortedSet_Lexicographic(t *testing.T) {
	cacheInstance := cache.New()
	engineInstance := engine.New(cacheInstance)
	go engineInstance.Run()
	defer engineInstance.Stop()

	evalInstance := New(cacheInstance, engineInstance, "", "", nil, nil)
	ctx := clientctx.New()

	// Add members with same score - should sort lexicographically
	evalInstance.Evaluate(ctx, "ZADD", []string{"samescores", "1", "charlie", "1", "alice", "1", "bob"})

	res := evalInstance.Evaluate(ctx, "ZRANGE", []string{"samescores", "0", "-1"})
	if res.Err != nil {
		t.Fatalf("ZRANGE failed: %v", res.Err)
	}

	members := res.Value.([]interface{})
	if len(members) != 3 {
		t.Fatalf("Expected 3 members, got %d", len(members))
	}

	// Should be sorted lexicographically: alice, bob, charlie
	if members[0] != "alice" || members[1] != "bob" || members[2] != "charlie" {
		t.Errorf("Expected lexicographic order [alice, bob, charlie], got %v", members)
	}
}
