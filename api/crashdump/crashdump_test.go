package crashdump

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	ops "gocache/api/operations"
)

func TestWrite_Basic(t *testing.T) {
	dir := t.TempDir()
	path, err := Write("kaboom", []byte("goroutine 1 [running]\n..."), Options{
		Dir:       dir,
		Version:   "v0.1.0",
		BootStage: "config_load",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	results, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Scan returned %d, want 1", len(results))
	}
	d := results[0].Dump
	if d.PanicValue != "kaboom" {
		t.Errorf("PanicValue = %q, want kaboom", d.PanicValue)
	}
	if d.Version != "v0.1.0" {
		t.Errorf("Version = %q", d.Version)
	}
	if d.BootStage != "config_load" {
		t.Errorf("BootStage = %q", d.BootStage)
	}
	if d.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", d.PID, os.Getpid())
	}
}

func TestWrite_WithActiveOps(t *testing.T) {
	dir := t.TempDir()
	op := ops.New(ops.TypePluginStart, "")
	op.Enrich("_plugin", "testy")

	_, err := Write("x", []byte("stack"), Options{
		Dir:       dir,
		ActiveOps: []*ops.Operation{op},
	})
	if err != nil {
		t.Fatal(err)
	}
	results, _ := Scan(dir)
	if len(results) != 1 {
		t.Fatalf("got %d results", len(results))
	}
	if len(results[0].Dump.ActiveOps) != 1 {
		t.Fatalf("ActiveOps = %d, want 1", len(results[0].Dump.ActiveOps))
	}
	if got := results[0].Dump.ActiveOps[0].Context["_plugin"]; got != "testy" {
		t.Errorf("context[_plugin] = %q, want testy", got)
	}
}

func TestScan_OrderAndFilter(t *testing.T) {
	dir := t.TempDir()

	// Write a non-crash file that should be filtered out.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two dumps with a guaranteed time ordering.
	_, _ = Write("first", []byte("stack"), Options{Dir: dir})
	time.Sleep(2 * time.Millisecond)
	_, _ = Write("second", []byte("stack"), Options{Dir: dir})

	results, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 dumps, got %d", len(results))
	}
	if results[0].Dump.PanicValue != "first" || results[1].Dump.PanicValue != "second" {
		t.Errorf("order wrong: %q, %q", results[0].Dump.PanicValue, results[1].Dump.PanicValue)
	}
}

func TestScan_MissingDirIsEmpty(t *testing.T) {
	results, err := Scan(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Errorf("expected nil error on missing dir, got %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %d", len(results))
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	path, _ := Write("x", []byte("stack"), Options{Dir: dir})

	if err := Delete(path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after Delete")
	}
	// Idempotent.
	if err := Delete(path); err != nil {
		t.Errorf("second Delete errored: %v", err)
	}
}

func TestWriteFromPanic(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteFromPanic("synthetic", Options{Dir: dir})
	if err != nil {
		t.Fatalf("WriteFromPanic: %v", err)
	}
	results, _ := Scan(dir)
	if len(results) != 1 {
		t.Fatalf("got %d results", len(results))
	}
	if results[0].Path != path {
		t.Errorf("path mismatch: %q vs %q", results[0].Path, path)
	}
	if !contains(results[0].Dump.Stack, "TestWriteFromPanic") {
		t.Errorf("stack should include caller frame, got:\n%s", results[0].Dump.Stack)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
