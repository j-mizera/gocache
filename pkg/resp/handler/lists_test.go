package handler_test

import (
	"testing"
)

func TestEvaluator_Lists(t *testing.T) {
	c, e, ctx := setup(t)

	// Test LPUSH/RPUSH and LLEN
	res := eval(t, c, e, ctx, "LPUSH", []string{"mylist", "world"})
	if res.Value != 1 {
		t.Errorf("expected 1, got %v", res.Value)
	}
	res = eval(t, c, e, ctx, "LPUSH", []string{"mylist", "hello"})
	if res.Value != 2 {
		t.Errorf("expected 2, got %v", res.Value)
	}
	res = eval(t, c, e, ctx, "RPUSH", []string{"mylist", "redis"})
	if res.Value != 3 {
		t.Errorf("expected 3, got %v", res.Value)
	}

	// Test LRANGE
	res = eval(t, c, e, ctx, "LRANGE", []string{"mylist", "0", "-1"})
	list, ok := res.Value.([]string)
	if !ok || len(list) != 3 || list[0] != "hello" || list[1] != "world" || list[2] != "redis" {
		t.Errorf("expected [hello world redis], got %v", res.Value)
	}

	// Test LPOP
	res = eval(t, c, e, ctx, "LPOP", []string{"mylist"})
	if res.Value != "hello" {
		t.Errorf("expected hello, got %v", res.Value)
	}

	// Test RPOP
	res = eval(t, c, e, ctx, "RPOP", []string{"mylist"})
	if res.Value != "redis" {
		t.Errorf("expected redis, got %v", res.Value)
	}

	res = eval(t, c, e, ctx, "LLEN", []string{"mylist"})
	if res.Value != 1 {
		t.Errorf("expected 1, got %v", res.Value)
	}
}
