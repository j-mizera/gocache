package handler_test

import (
	"os"
	"path/filepath"
	"testing"

	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/persistence"
	"gocache/pkg/resp/handler"
)

// TestHandler_Snapshot exercises SAVE through the coordinator + a
// registered snapshotter. The plugin owns format/filename — we use the
// gob shim here as a stand-in for the registered persistence plugin so
// the test stays plugin-agnostic. (The real binary uses the snapshot plugin; the
// gob shim still implements both Source and Snapshotter for tests.)
func TestHandler_Snapshot(t *testing.T) {
	dir := t.TempDir()
	snapshotFile := filepath.Join(dir, "test_handler_snapshot.dat")

	c1 := cache.New()
	e1 := engine.New(c1)
	go e1.Run()
	t.Cleanup(func() { e1.Stop() })
	ctx1 := clientctx.New()

	res := eval(t, c1, e1, ctx1, "SET", []string{"snap", "data"})
	if res.Value != "OK" {
		t.Fatalf("SET: %v", res.Value)
	}

	gob := persistence.NewGobSource(snapshotFile)
	coord := persistence.New(gob)
	coord.RegisterSnapshotter(gob)

	cmdCtx := &command.Context{
		Client:      ctx1,
		Op:          "SNAPSHOT",
		Engine:      e1,
		Cache:       c1,
		Snapshotter: coord,
	}
	res = handler.HandleSnapshot(cmdCtx)
	if res.Value != "OK" {
		t.Fatalf("SNAPSHOT: %v", res.Value)
	}

	if _, err := os.Stat(snapshotFile); os.IsNotExist(err) {
		t.Fatalf("%s was not created", snapshotFile)
	}
}
