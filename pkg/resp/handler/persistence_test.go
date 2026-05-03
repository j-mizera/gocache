package handler_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	apipersistence "gocache/api/persistence"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/persistence"
	"gocache/pkg/persistence/v1snap"
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

	// SNAPSHOT — needs Snapshotter wired through the coordinator. The
	// gob shim implements both Source (boot side) and Snapshotter
	// (runtime save side).
	gob := persistence.NewGobSource(snapshotFile)
	coord := persistence.New(gob)
	coord.RegisterSnapshotter(gob)

	cmdCtx := &command.Context{
		Client:       ctx1,
		Op:           "SNAPSHOT",
		Engine:       e1,
		Cache:        c1,
		SnapshotFile: snapshotFile,
		Snapshotter:  coord,
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
	if valueAsString(res.Value) != "data" {
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

// TestHandler_LoadSnapshot_V1 verifies that LOAD_SNAPSHOT auto-detects
// the v1 binary format and loads it correctly. The runtime command
// path doesn't go through the coordinator's startup format selection,
// so format detection has to live inside the handler — this test
// guards the dual-format contract.
func TestHandler_LoadSnapshot_V1(t *testing.T) {
	dir := t.TempDir()
	v1file := filepath.Join(dir, "v1.snap")

	w := v1snap.NewSnapshotter(v1file)
	src := &sliceSrcForTest{entries: []apipersistence.SnapshotEntry{
		{Key: "v1key", ValueType: apipersistence.ValueTypeBytes, Encoding: apipersistence.EncodingNative, Value: []byte("v1val")},
	}}
	if err := w.SaveSnapshot(context.Background(), src); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	c := cache.New()
	e := engine.New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })

	loadCtx := &command.Context{
		Client:       clientctx.New(),
		Op:           "LOAD_SNAPSHOT",
		Args:         []string{filepath.Base(v1file)},
		Engine:       e,
		Cache:        c,
		SnapshotFile: v1file,
	}
	res := handler.HandleLoadSnapshot(loadCtx)
	if res.Value != "OK" {
		t.Fatalf("LOAD_SNAPSHOT v1: %v / err=%v", res.Value, res.Err)
	}

	got := eval(t, c, e, clientctx.New(), "GET", []string{"v1key"})
	if valueAsString(got.Value) != "v1val" {
		t.Errorf("GET v1key after load: got %v, want v1val", got.Value)
	}
}

// sliceSrcForTest is a minimal SnapshotSource for the v1 load test.
type sliceSrcForTest struct {
	entries []apipersistence.SnapshotEntry
	cursor  int
}

func (s *sliceSrcForTest) Next(_ context.Context) (apipersistence.SnapshotEntry, error) {
	if s.cursor >= len(s.entries) {
		return apipersistence.SnapshotEntry{}, io.EOF
	}
	e := s.entries[s.cursor]
	s.cursor++
	return e, nil
}
