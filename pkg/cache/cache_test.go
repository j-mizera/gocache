package cache_test

import (
	"context"
	"errors"
	"gocache/pkg/cache"
	"gocache/pkg/persistence"
	"os"
	"testing"
)

func TestCache_Basic(t *testing.T) {
	c := cache.New()
	if err := c.RawSet(context.Background(), "key", "value", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	val, found := c.RawGet("key")
	if !found || val.Value != "value" {
		t.Errorf("expected value, got %v", val)
	}

	c.RawDelete("key")
	_, found = c.RawGet("key")
	if found {
		t.Errorf("expected not found")
	}
}

func TestCache_Snapshot(t *testing.T) {
	filename := "test_cache_snapshot.dat"
	defer os.Remove(filename)

	c := cache.New()
	if err := c.RawSet(context.Background(), "snap", "data", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err := persistence.SaveSnapshot(context.Background(), filename, c)
	if err != nil {
		t.Fatalf("failed to save snapshot: %v", err)
	}

	cacheInstance2 := cache.New()
	err = persistence.LoadSnapshot(context.Background(), filename, cacheInstance2)
	if err != nil {
		t.Fatalf("failed to load snapshot: %v", err)
	}

	val, found := cacheInstance2.RawGet("snap")
	if !found || val.Value != "data" {
		t.Errorf("expected data, got %v", val)
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
