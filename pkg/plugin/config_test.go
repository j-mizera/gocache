package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscover_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	cfg := PluginsConfig{Dir: dir}

	plugins, err := Discover(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plugins) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(plugins))
	}
}

func TestDiscover_NonexistentDir(t *testing.T) {
	cfg := PluginsConfig{Dir: "/nonexistent/path"}

	plugins, err := Discover(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plugins != nil {
		t.Error("expected nil for nonexistent dir")
	}
}

func TestDiscover_FindsExecutables(t *testing.T) {
	dir := t.TempDir()

	// Create executable file.
	execPath := filepath.Join(dir, "myplugin")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// Create non-executable file (should be skipped).
	nonExecPath := filepath.Join(dir, "readme.txt")
	if err := os.WriteFile(nonExecPath, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create directory (should be skipped).
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	cfg := PluginsConfig{Dir: dir}
	plugins, err := Discover(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	if plugins[0].Name != "myplugin" {
		t.Errorf("expected name 'myplugin', got %q", plugins[0].Name)
	}
	if plugins[0].BinPath != execPath {
		t.Errorf("expected path %q, got %q", execPath, plugins[0].BinPath)
	}
}

func TestDiscover_AppliesOverrides(t *testing.T) {
	dir := t.TempDir()

	execPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := PluginsConfig{
		Dir: dir,
		Overrides: map[string]PluginOverride{
			"auth": {Critical: true, Priority: 1},
		},
	}

	plugins, err := Discover(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	if !plugins[0].Critical {
		t.Error("expected critical=true from override")
	}
	if plugins[0].Priority != 1 {
		t.Errorf("expected priority=1, got %d", plugins[0].Priority)
	}
}

func TestDiscover_BinaryOverride(t *testing.T) {
	dir := t.TempDir()

	execPath := filepath.Join(dir, "metrics")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := PluginsConfig{
		Dir: dir,
		Overrides: map[string]PluginOverride{
			"metrics": {Binary: "/custom/path/metrics-v2"},
		},
	}

	plugins, err := Discover(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plugins[0].BinPath != "/custom/path/metrics-v2" {
		t.Errorf("expected custom binary path, got %q", plugins[0].BinPath)
	}
}

func TestDiscover_EmptyDirConfig(t *testing.T) {
	cfg := PluginsConfig{Dir: ""}
	plugins, err := Discover(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plugins != nil {
		t.Error("expected nil for empty dir config")
	}
}
