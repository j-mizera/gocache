package bootstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boot.state")

	if err := Write(path, "config_load"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	s, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.Stage != "config_load" {
		t.Errorf("Stage = %q, want config_load", s.Stage)
	}
	if s.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", s.PID, os.Getpid())
	}
	if s.Timestamp.IsZero() {
		t.Error("Timestamp is zero")
	}
}

func TestWrite_AtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boot.state")

	if err := Write(path, "stage_a"); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, "stage_b"); err != nil {
		t.Fatal(err)
	}
	s, _ := Read(path)
	if s.Stage != "stage_b" {
		t.Errorf("stage = %q, want stage_b", s.Stage)
	}
}

func TestRead_NotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.state")

	_, err := Read(path)
	if !os.IsNotExist(err) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boot.state")
	if err := Write(path, "x"); err != nil {
		t.Fatal(err)
	}
	if err := Clear(path); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := Read(path); !os.IsNotExist(err) {
		t.Errorf("expected not exist after Clear, got %v", err)
	}
	// Clear of a nonexistent file should not error.
	if err := Clear(path); err != nil {
		t.Errorf("Clear on missing file errored: %v", err)
	}
}

func TestWrite_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "boot.state")

	if err := Write(path, "early"); err != nil {
		t.Fatalf("Write should create parent dirs: %v", err)
	}
	s, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if s.Stage != "early" {
		t.Errorf("Stage = %q, want early", s.Stage)
	}
}
