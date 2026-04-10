package handler_test

import (
	"os"
	"testing"

	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/resp/handler"
)

func TestHandler_Snapshot(t *testing.T) {
	snapshotFile := "test_handler_snapshot.dat"
	filename := "test_handler_snapshot_2.dat"
	defer os.Remove(snapshotFile)
	defer os.Remove(filename)

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
	os.Rename(snapshotFile, filename)

	// Load into fresh cache
	c2 := cache.New()
	e2 := engine.New(c2)
	go e2.Run()
	t.Cleanup(func() { e2.Stop() })
	ctx2 := clientctx.New()

	res = eval(t, c2, e2, ctx2, "LOAD_SNAPSHOT", []string{filename})
	if res.Value != "OK" {
		t.Fatalf("LOAD_SNAPSHOT: %v", res.Value)
	}

	res = eval(t, c2, e2, ctx2, "GET", []string{"snap"})
	if res.Value != "data" {
		t.Errorf("GET snap: expected data, got %v", res.Value)
	}
}
