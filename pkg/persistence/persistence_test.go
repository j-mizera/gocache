package persistence

import (
	"gocache/pkg/cache"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "snapshot.dat")

	c := cache.New()
	c.Lock()
	_ = c.RawSet("str", "hello", 0)
	_ = c.RawSet("list", []string{"a", "b", "c"}, 0)
	_ = c.RawSet("hash", map[string]string{"k": "v"}, 0)
	_ = c.RawSet("set", map[string]struct{}{"x": {}, "y": {}}, 0)
	c.Unlock()

	if err := SaveSnapshot(file, c); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	c2 := cache.New()
	if err := LoadSnapshot(file, c2); err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}

	c2.Lock()
	defer c2.Unlock()

	entry, ok := c2.RawGet("str")
	if !ok || entry.Value != "hello" {
		t.Errorf("str: expected 'hello', got %v", entry)
	}

	entry, ok = c2.RawGet("list")
	if !ok {
		t.Fatal("list not found")
	}
	list, _ := entry.Value.([]string)
	if len(list) != 3 {
		t.Errorf("list: expected 3 items, got %d", len(list))
	}

	if c2.Len() != 4 {
		t.Errorf("expected 4 keys, got %d", c2.Len())
	}
}

func TestLoadSnapshot_FileNotFound(t *testing.T) {
	c := cache.New()
	err := LoadSnapshot("/nonexistent/path/file.dat", c)
	if err != nil {
		t.Errorf("expected nil for missing file, got %v", err)
	}
}

func TestLoadSnapshot_SkipsExpired(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "snapshot.dat")

	c := cache.New()
	c.Lock()
	// Set a key that's already expired.
	_ = c.RawSet("expired", "val", time.Now().Add(-time.Hour).UnixNano())
	_ = c.RawSet("alive", "val", 0)
	c.Unlock()

	if err := SaveSnapshot(file, c); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	c2 := cache.New()
	if err := LoadSnapshot(file, c2); err != nil {
		t.Fatalf("LoadSnapshot failed: %v", err)
	}

	c2.Lock()
	defer c2.Unlock()

	if _, ok := c2.RawGet("expired"); ok {
		t.Error("expected expired key to be skipped")
	}
	if _, ok := c2.RawGet("alive"); !ok {
		t.Error("expected alive key to be loaded")
	}
}

func TestSaveSnapshot_CreateError(t *testing.T) {
	err := SaveSnapshot("/nonexistent/dir/file.dat", cache.New())
	if err == nil {
		t.Error("expected error for invalid path")
	}
}

func TestLoadSnapshot_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "empty.dat")
	_ = os.WriteFile(file, []byte{}, 0644)

	c := cache.New()
	err := LoadSnapshot(file, c)
	if err == nil {
		t.Error("expected error for empty file")
	}
}
