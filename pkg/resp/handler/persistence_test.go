package handler_test

import (
	"os"
	"path/filepath"
	"testing"

	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/resp/handler"
)

func TestHandler_Snapshot(t *testing.T) {
	// Use a temp dir so the LOAD_SNAPSHOT path-traversal guard has a
	// well-defined base directory.
	dir := t.TempDir()
	snapshotFile := filepath.Join(dir, "test_handler_snapshot.dat")

	c1 := cache.New()
	e1 := engine.New(c1)
	go e1.Run()
	t.Cleanup(func() { e1.Stop() })
	ctx1 := clientctx.New()

	// SET a value
	res := eval(t, c1, e1, ctx1, "SET", []string{"snap", "data"})
	if res.Value != "OK" {
		t.Fatalf("SET: %v", res.Value)
	}

	// SNAPSHOT -- needs SnapshotFile on context
	cmdCtx := &command.Context{
		Client:       ctx1,
		Op:           "SNAPSHOT",
		Engine:       e1,
		Cache:        c1,
		SnapshotFile: snapshotFile,
	}
	res = handler.HandleSnapshot(cmdCtx)
	if res.Value != "OK" {
		t.Fatalf("SNAPSHOT: %v", res.Value)
	}

	if _, err := os.Stat(snapshotFile); os.IsNotExist(err) {
		t.Fatalf("%s was not created", snapshotFile)
	}

	// Load into fresh cache. Call HandleLoadSnapshot directly so we can set
	// SnapshotFile (eval() uses a minimal command.Context without config).
	c2 := cache.New()
	e2 := engine.New(c2)
	go e2.Run()
	t.Cleanup(func() { e2.Stop() })
	ctx2 := clientctx.New()

	loadCtx := &command.Context{
		Client:       ctx2,
		Op:           "LOAD_SNAPSHOT",
		Args:         []string{filepath.Base(snapshotFile)},
		Engine:       e2,
		Cache:        c2,
		SnapshotFile: snapshotFile,
	}
	res = handler.HandleLoadSnapshot(loadCtx)
	if res.Value != "OK" {
		t.Fatalf("LOAD_SNAPSHOT: %v", res.Value)
	}

	res = eval(t, c2, e2, ctx2, "GET", []string{"snap"})
	if res.Value != "data" {
		t.Errorf("GET snap: expected data, got %v", res.Value)
	}
}

func TestHandler_LoadSnapshot_PathTraversal(t *testing.T) {
	// Verify that LOAD_SNAPSHOT rejects absolute paths, parent traversal,
	// and subpaths that escape the base snapshot directory.
	dir := t.TempDir()
	baseSnapshot := filepath.Join(dir, "ok.snap")

	c := cache.New()
	e := engine.New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })

	bad := []string{
		"/etc/passwd",
		"../../etc/passwd",
		"sub/../../escape",
	}
	for _, arg := range bad {
		loadCtx := &command.Context{
			Client:       clientctx.New(),
			Op:           "LOAD_SNAPSHOT",
			Args:         []string{arg},
			Engine:       e,
			Cache:        c,
			SnapshotFile: baseSnapshot,
		}
		res := handler.HandleLoadSnapshot(loadCtx)
		if res.Value == "OK" {
			t.Errorf("path %q should have been rejected, got OK", arg)
		}
	}
}
