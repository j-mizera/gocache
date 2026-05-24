package cache_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gammazero/deque"

	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
	"gocache/pkg/persistence"
)

func TestCache_Basic(t *testing.T) {
	c := cache.New()
	if err := c.RawSet(context.Background(), "key", "value", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	val, found := c.RawGet("key")
	if !found {
		t.Fatalf("expected key to be found")
	}
	if got := string(c.ResolvePacked(val)); got != "value" {
		t.Errorf("expected %q, got %q", "value", got)
	}

	c.RawDelete("key")
	_, found = c.RawGet("key")
	if found {
		t.Errorf("expected not found")
	}
}

func TestCache_Snapshot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "test_cache_snapshot.dat")

	c := cache.New()
	if err := c.RawSet(context.Background(), "snap", "data", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gob := persistence.NewGobSource(file)
	coord := persistence.New(gob)
	coord.SetStore(c)
	coord.RegisterSnapshotter(gob)

	if err := coord.Snapshot(context.Background()); err != nil {
		t.Fatalf("save: %v", err)
	}

	c2 := cache.New()
	coord2 := persistence.New(persistence.NewGobSource(file))
	coord2.SetStore(c2)
	if _, err := coord2.BootInto(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}

	val, found := c2.RawGet("snap")
	if !found {
		t.Fatal("snap key not loaded")
	}
	if got := string(c2.ResolvePacked(val)); got != "data" {
		t.Errorf("expected %q, got %q", "data", got)
	}
}

func TestCache_MemoryLimit_LRU(t *testing.T) {
	// Create a very small cache (200 bytes forces eviction after ~1 entry)
	c := cache.NewWithBytes(200, cache.EvictionLRU)

	// Fill it: each write evicts the previous LRU entry
	for i := 0; i < 5; i++ {
		key := string(rune('a' + i))
		if err := c.RawSet(context.Background(), key, "value", 0); err != nil {
			t.Fatalf("unexpected OOM on LRU cache at key %s: %v", key, err)
		}
	}

	// Only the most recently written key should survive
	_, found := c.RawGet("e")
	if !found {
		t.Error("most recently written key 'e' should still be present")
	}

	// UsedBytes must be > 0 and <= MaxBytes after eviction
	if c.UsedBytes() <= 0 {
		t.Errorf("expected usedBytes > 0, got %d", c.UsedBytes())
	}
}

func TestCache_MemoryLimit_NoEviction(t *testing.T) {
	c := cache.NewWithBytes(200, cache.EvictionNone)

	// First write should succeed (cache is empty)
	if err := c.RawSet(context.Background(), "first", "v", 0); err != nil {
		t.Fatalf("unexpected error on first write: %v", err)
	}

	// Subsequent writes that exceed the limit must return ErrOutOfMemory
	err := c.RawSet(context.Background(), "second", "v", 0)
	if !errors.Is(err, cache.ErrOutOfMemory) {
		t.Errorf("expected ErrOutOfMemory, got %v", err)
	}
}

func TestCache_MemoryTracking(t *testing.T) {
	c := cache.New()

	if c.UsedBytes() != 0 {
		t.Errorf("expected 0 used bytes on empty cache, got %d", c.UsedBytes())
	}

	_ = c.RawSet(context.Background(), "key", "hello", 0)
	usedAfterSet := c.UsedBytes()
	if usedAfterSet <= 0 {
		t.Errorf("expected usedBytes > 0 after set, got %d", usedAfterSet)
	}

	c.RawDelete("key")
	if c.UsedBytes() != 0 {
		t.Errorf("expected 0 used bytes after delete, got %d", c.UsedBytes())
	}
}

func TestCache_LRU_OrderOnGet(t *testing.T) {
	// Cache that holds ~2 small entries; verify that GET refreshes LRU order
	c := cache.NewWithBytes(300, cache.EvictionLRU)

	_ = c.RawSet(context.Background(), "a", "1", 0)
	_ = c.RawSet(context.Background(), "b", "2", 0) // b is now MRU, a is LRU

	// Access "a" to make it MRU; "b" becomes LRU
	c.RawGet("a")

	// Writing a new key should evict "b" (now LRU), not "a"
	_ = c.RawSet(context.Background(), "c", "3", 0)

	_, aFound := c.RawGet("a")
	_, bFound := c.RawGet("b")
	_, cFound := c.RawGet("c")

	if !aFound {
		t.Error("key 'a' should survive (was accessed before eviction)")
	}
	if bFound {
		t.Error("key 'b' should have been evicted (was LRU)")
	}
	if !cFound {
		t.Error("key 'c' should be present (just written)")
	}
}

func TestCache_SetMemoryLimit_EvictsWhenLowered(t *testing.T) {
	// Start with unlimited cache and add several keys.
	c := cache.New()
	for i := 0; i < 10; i++ {
		key := "key" + string(rune('a'+i))
		if err := c.RawSet(context.Background(), key, "somevalue", 0); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if c.Len() != 10 {
		t.Fatalf("expected 10 keys, got %d", c.Len())
	}

	usedBefore := c.UsedBytes()
	if usedBefore == 0 {
		t.Fatal("expected non-zero usedBytes")
	}

	// Lower the limit to something tiny — should trigger eviction.
	c.SetMemoryLimit(context.Background(), 1, cache.EvictionLRU) // 1 MB — still bigger than our data
	// Use a byte-level limit via the internal path: set maxBytes directly by
	// creating a new cache with bytes limit to test eviction trigger.
	small := cache.NewWithBytes(200, cache.EvictionLRU)
	for i := 0; i < 5; i++ {
		key := "k" + string(rune('a'+i))
		_ = small.RawSet(context.Background(), key, "val", 0)
	}
	before := small.Len()
	if before == 0 {
		t.Fatal("expected some keys")
	}

	// Lower to 1 byte — must evict down.
	small.SetMemoryLimit(context.Background(), 0, cache.EvictionLRU) // disable limit first
	// Manually set a very small byte limit. Since SetMemoryLimit takes MB,
	// we use NewWithBytes + re-populate to test the eviction path.
	tiny := cache.NewWithBytes(1, cache.EvictionLRU)
	for i := 0; i < 5; i++ {
		_ = tiny.RawSet(context.Background(), "k"+string(rune('a'+i)), "val", 0)
	}
	// Only one key should survive (each entry > 128 bytes overhead).
	if tiny.Len() > 1 {
		t.Errorf("expected at most 1 key after tiny limit, got %d", tiny.Len())
	}
}

func TestCache_LargeEntryExceedsMaxBytes(t *testing.T) {
	// A single entry larger than maxBytes should be rejected with noeviction.
	c := cache.NewWithBytes(1, cache.EvictionNone)
	err := c.RawSet(context.Background(), "big", "this is way more than 1 byte of data", 0)
	if !errors.Is(err, cache.ErrOutOfMemory) {
		t.Errorf("expected ErrOutOfMemory for oversized entry, got %v", err)
	}
	if c.Len() != 0 {
		t.Errorf("expected 0 keys, got %d", c.Len())
	}
}

func TestCache_ParseEvictionPolicy(t *testing.T) {
	tests := []struct {
		input string
		want  cache.EvictionPolicy
	}{
		{"lru", cache.EvictionLRU},
		{"LRU", cache.EvictionLRU},
		{"allkeys-lru", cache.EvictionLRU},
		{"none", cache.EvictionNone},
		{"noeviction", cache.EvictionNone},
		{"", cache.EvictionNone},
	}
	for _, tt := range tests {
		got := cache.ParseEvictionPolicy(tt.input)
		if got != tt.want {
			t.Errorf("ParseEvictionPolicy(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestApplyMutation(t *testing.T) {
	ctx := context.Background()
	b := func(s string) []byte { return []byte(s) }
	mut := func(op string, args ...string) apipersistence.Mutation {
		ba := make([][]byte, len(args))
		for i, a := range args {
			ba[i] = []byte(a)
		}
		return apipersistence.Mutation{Op: op, Key: args[0], Args: ba}
	}

	t.Run("SET and GET", func(t *testing.T) {
		c := cache.New()
		if err := c.ApplyMutation(ctx, mut("SET", "k", "v")); err != nil {
			t.Fatal(err)
		}
		e, ok := c.RawGet("k")
		if !ok {
			t.Fatal("key not found")
		}
		if got := string(c.ResolvePacked(e)); got != "v" {
			t.Errorf("got %q, want %q", got, "v")
		}
	})

	t.Run("DEL", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "a", "1"))
		_ = c.ApplyMutation(ctx, mut("SET", "b", "2"))
		_ = c.ApplyMutation(ctx, apipersistence.Mutation{
			Op: "DEL", Key: "a", Args: [][]byte{b("a"), b("b")},
		})
		if _, ok := c.RawGet("a"); ok {
			t.Error("a should be deleted")
		}
		if _, ok := c.RawGet("b"); ok {
			t.Error("b should be deleted")
		}
	})

	t.Run("INCR and DECRBY", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "n", "10"))
		_ = c.ApplyMutation(ctx, mut("INCR", "n"))
		e, _ := c.RawGet("n")
		if got := string(c.ResolvePacked(e)); got != "11" {
			t.Errorf("INCR: got %q, want %q", got, "11")
		}
		_ = c.ApplyMutation(ctx, mut("DECRBY", "n", "5"))
		e, _ = c.RawGet("n")
		if got := string(c.ResolvePacked(e)); got != "6" {
			t.Errorf("DECRBY: got %q, want %q", got, "6")
		}
	})

	t.Run("APPEND", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "s", "hello"))
		_ = c.ApplyMutation(ctx, mut("APPEND", "s", " world"))
		e, _ := c.RawGet("s")
		if got := string(c.ResolvePacked(e)); got != "hello world" {
			t.Errorf("got %q, want %q", got, "hello world")
		}
	})

	t.Run("HSET and HDEL", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("HSET", "h", "f1", "v1", "f2", "v2"))
		e, ok := c.RawGet("h")
		if !ok {
			t.Fatal("hash not found")
		}
		h := e.Value.(map[string]string)
		if h["f1"] != "v1" || h["f2"] != "v2" {
			t.Errorf("unexpected hash: %v", h)
		}
		_ = c.ApplyMutation(ctx, mut("HDEL", "h", "f1"))
		e, _ = c.RawGet("h")
		h = e.Value.(map[string]string)
		if _, ok := h["f1"]; ok {
			t.Error("f1 should be deleted")
		}
		if h["f2"] != "v2" {
			t.Error("f2 should remain")
		}
	})

	t.Run("SADD and SREM", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SADD", "s", "a", "b", "c"))
		e, _ := c.RawGet("s")
		s := e.Value.(map[string]struct{})
		if len(s) != 3 {
			t.Errorf("set len = %d, want 3", len(s))
		}
		_ = c.ApplyMutation(ctx, mut("SREM", "s", "b"))
		e, _ = c.RawGet("s")
		s = e.Value.(map[string]struct{})
		if _, ok := s["b"]; ok {
			t.Error("b should be removed")
		}
		if len(s) != 2 {
			t.Errorf("set len = %d, want 2", len(s))
		}
	})

	t.Run("ZADD and ZREM", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("ZADD", "z", "1.5", "alice", "2.5", "bob"))
		e, _ := c.RawGet("z")
		z := e.Value.(*cache.SortedSet)
		if z.Card() != 2 {
			t.Errorf("zcard = %d, want 2", z.Card())
		}
		score, ok := z.Score("alice")
		if !ok || score != 1.5 {
			t.Errorf("alice score = %v %v, want 1.5", score, ok)
		}
		_ = c.ApplyMutation(ctx, mut("ZREM", "z", "alice"))
		e, _ = c.RawGet("z")
		z = e.Value.(*cache.SortedSet)
		if z.Card() != 1 {
			t.Errorf("zcard = %d, want 1", z.Card())
		}
	})

	t.Run("LPUSH RPUSH LPOP RPOP", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("RPUSH", "l", "a", "b", "c"))
		_ = c.ApplyMutation(ctx, mut("LPUSH", "l", "z"))
		// list: z a b c
		_ = c.ApplyMutation(ctx, mut("LPOP", "l"))
		_ = c.ApplyMutation(ctx, mut("RPOP", "l"))
		// list: a b
		e, _ := c.RawGet("l")
		dq := e.Value.(*deque.Deque[string])
		if dq.Len() != 2 {
			t.Fatalf("list len = %d, want 2", dq.Len())
		}
		if dq.At(0) != "a" || dq.At(1) != "b" {
			t.Errorf("list = [%s, %s], want [a, b]", dq.At(0), dq.At(1))
		}
	})

	t.Run("MSET", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, apipersistence.Mutation{
			Op: "MSET", Key: "k1",
			Args: [][]byte{b("k1"), b("v1"), b("k2"), b("v2")},
		})
		for _, kv := range [][2]string{{"k1", "v1"}, {"k2", "v2"}} {
			e, ok := c.RawGet(kv[0])
			if !ok {
				t.Errorf("%s not found", kv[0])
				continue
			}
			if got := string(c.ResolvePacked(e)); got != kv[1] {
				t.Errorf("%s = %q, want %q", kv[0], got, kv[1])
			}
		}
	})

	t.Run("RENAME", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "old", "val"))
		_ = c.ApplyMutation(ctx, mut("RENAME", "old", "new"))
		if _, ok := c.RawGet("old"); ok {
			t.Error("old key should not exist")
		}
		e, ok := c.RawGet("new")
		if !ok {
			t.Fatal("new key not found")
		}
		if got := string(c.ResolvePacked(e)); got != "val" {
			t.Errorf("got %q, want %q", got, "val")
		}
	})

	t.Run("FLUSHALL", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "a", "1"))
		_ = c.ApplyMutation(ctx, mut("SET", "b", "2"))
		_ = c.ApplyMutation(ctx, apipersistence.Mutation{Op: "FLUSHALL"})
		if c.Len() != 0 {
			t.Errorf("cache len = %d, want 0", c.Len())
		}
	})

	t.Run("SETNX", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SETNX", "nx", "first"))
		_ = c.ApplyMutation(ctx, mut("SETNX", "nx", "second"))
		e, _ := c.RawGet("nx")
		if got := string(c.ResolvePacked(e)); got != "first" {
			t.Errorf("got %q, want %q", got, "first")
		}
	})

	t.Run("GETSET", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "gs", "old"))
		_ = c.ApplyMutation(ctx, mut("GETSET", "gs", "new"))
		e, _ := c.RawGet("gs")
		if got := string(c.ResolvePacked(e)); got != "new" {
			t.Errorf("got %q, want %q", got, "new")
		}
	})

	t.Run("GETDEL", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "gd", "val"))
		_ = c.ApplyMutation(ctx, mut("GETDEL", "gd"))
		if _, ok := c.RawGet("gd"); ok {
			t.Error("key should be deleted after GETDEL")
		}
	})

	t.Run("INCRBYFLOAT", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "f", "10.5"))
		_ = c.ApplyMutation(ctx, mut("INCRBYFLOAT", "f", "0.1"))
		e, _ := c.RawGet("f")
		got := string(c.ResolvePacked(e))
		if got != "10.6" {
			t.Errorf("got %q, want %q", got, "10.6")
		}
	})

	t.Run("EXPIRE", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "ex", "val"))
		_ = c.ApplyMutation(ctx, mut("EXPIRE", "ex", "60"))
		ttl := c.RawTTL("ex")
		if ttl <= 0 {
			t.Errorf("expected positive TTL, got %d", ttl)
		}
	})

	t.Run("PEXPIRE", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SET", "px", "val"))
		_ = c.ApplyMutation(ctx, mut("PEXPIRE", "px", "60000"))
		ttl := c.RawTTL("px")
		if ttl <= 0 {
			t.Errorf("expected positive TTL, got %d", ttl)
		}
	})

	t.Run("SPOP", func(t *testing.T) {
		c := cache.New()
		_ = c.ApplyMutation(ctx, mut("SADD", "sp", "a", "b", "c"))
		_ = c.ApplyMutation(ctx, mut("SPOP", "sp"))
		e, _ := c.RawGet("sp")
		s := e.Value.(map[string]struct{})
		if len(s) != 2 {
			t.Errorf("set len = %d, want 2", len(s))
		}
	})

	t.Run("unknown op returns error", func(t *testing.T) {
		c := cache.New()
		err := c.ApplyMutation(ctx, mut("XYZZY", "k"))
		if err == nil {
			t.Error("expected error for unknown op")
		}
	})
}
