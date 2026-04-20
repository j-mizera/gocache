package handler_test

import (
	"strings"
	"testing"

	"gocache/pkg/cache"
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

// TestList_EncodingPromotion verifies a list starts as EncPacked when
// small and transparently promotes to EncNative once the encoded size
// crosses ListMaxBytes. Pops continue to work after promotion.
func TestList_EncodingPromotion(t *testing.T) {
	c, e, ctx := setup(t)
	// Tight threshold: ~30 bytes — two 10-byte strings with framing will
	// push us over.
	c.SetPackedThresholds(cache.PackedThresholds{ListMaxBytes: 30})

	eval(t, c, e, ctx, "RPUSH", []string{"l", "aaaaaaaaaa"})
	entry, _ := c.RawGet("l")
	if entry.Encoding != cache.EncPacked {
		t.Errorf("first push: Encoding = %v; want EncPacked", entry.Encoding)
	}

	// Repeated pushes accumulate until promotion.
	for i := 0; i < 5; i++ {
		eval(t, c, e, ctx, "RPUSH", []string{"l", strings.Repeat("b", 10)})
	}
	entry, _ = c.RawGet("l")
	if entry.Encoding != cache.EncNative {
		t.Errorf("after growth: Encoding = %v; want EncNative", entry.Encoding)
	}

	// LLEN, LRANGE, LPOP all continue working post-promotion.
	res := eval(t, c, e, ctx, "LLEN", []string{"l"})
	if res.Value != 6 {
		t.Errorf("LLEN = %v; want 6", res.Value)
	}
	res = eval(t, c, e, ctx, "LPOP", []string{"l"})
	if res.Value != "aaaaaaaaaa" {
		t.Errorf("LPOP = %v; want aaaaaaaaaa", res.Value)
	}
}
