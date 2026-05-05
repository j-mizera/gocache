package handler_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/persistence"
	"gocache/pkg/resp/handler"
)

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

func TestHandler_Save(t *testing.T) {
	dir := t.TempDir()
	snapshotFile := filepath.Join(dir, "save.dat")

	c, e, ctx := setup(t)
	_ = eval(t, c, e, ctx, "SET", []string{"k", "v"})

	gob := persistence.NewGobSource(snapshotFile)
	coord := persistence.New(gob)
	coord.RegisterSnapshotter(gob)

	cmdCtx := &command.Context{
		Client:      ctx,
		Op:          "SAVE",
		Engine:      e,
		Cache:       c,
		Snapshotter: coord,
	}
	res := handler.HandleSave(cmdCtx)
	if res.Value != "OK" {
		t.Fatalf("SAVE: expected OK, got %v (err=%v)", res.Value, res.Err)
	}
}

func TestHandler_Save_NoSnapshotter(t *testing.T) {
	c, e, ctx := setup(t)

	cmdCtx := &command.Context{
		Client: ctx,
		Op:     "SAVE",
		Engine: e,
		Cache:  c,
	}
	res := handler.HandleSave(cmdCtx)
	if res.Err == nil {
		t.Fatal("SAVE with nil snapshotter: expected error")
	}
}

func TestHandler_Bgsave(t *testing.T) {
	dir := t.TempDir()
	snapshotFile := filepath.Join(dir, "bgsave.dat")

	c, e, ctx := setup(t)
	_ = eval(t, c, e, ctx, "SET", []string{"k", "v"})

	gob := persistence.NewGobSource(snapshotFile)
	coord := persistence.New(gob)
	coord.RegisterSnapshotter(gob)

	cmdCtx := &command.Context{
		Client:      ctx,
		Op:          "BGSAVE",
		Engine:      e,
		Cache:       c,
		Snapshotter: coord,
	}
	res := handler.HandleBgsave(cmdCtx)
	if res.Err != nil {
		t.Fatalf("BGSAVE: unexpected error: %v", res.Err)
	}
	if res.Value != "Background saving started" {
		t.Errorf("BGSAVE: got %v, want %q", res.Value, "Background saving started")
	}

	// Wait for background goroutine to finish writing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(snapshotFile); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("BGSAVE did not create snapshot file within 2s")
}

func TestHandler_Bgsave_NoSnapshotter(t *testing.T) {
	_, e, ctx := setup(t)

	cmdCtx := &command.Context{
		Client: ctx,
		Op:     "BGSAVE",
		Engine: e,
		Cache:  cache.New(),
	}
	res := handler.HandleBgsave(cmdCtx)
	if res.Err == nil {
		t.Fatal("BGSAVE with nil snapshotter: expected error")
	}
}

func TestHandler_Lastsave(t *testing.T) {
	dir := t.TempDir()
	snapshotFile := filepath.Join(dir, "lastsave.dat")

	c, e, ctx := setup(t)

	gob := persistence.NewGobSource(snapshotFile)
	coord := persistence.New(gob)
	coord.RegisterSnapshotter(gob)

	// Before any save, LASTSAVE should return 0.
	cmdCtx := &command.Context{
		Client:      ctx,
		Op:          "LASTSAVE",
		Engine:      e,
		Cache:       c,
		Snapshotter: coord,
	}
	res := handler.HandleLastsave(cmdCtx)
	if res.Err != nil {
		t.Fatalf("LASTSAVE: unexpected error: %v", res.Err)
	}
	if res.Value != int64(0) {
		t.Errorf("LASTSAVE before save = %v, want 0", res.Value)
	}

	// Do a save.
	before := time.Now().Unix()
	saveCmdCtx := &command.Context{
		Client:      ctx,
		Op:          "SAVE",
		Engine:      e,
		Cache:       c,
		Snapshotter: coord,
	}
	if r := handler.HandleSave(saveCmdCtx); r.Err != nil {
		t.Fatalf("SAVE: %v", r.Err)
	}
	after := time.Now().Unix()

	// LASTSAVE should now return a timestamp in [before, after].
	res = handler.HandleLastsave(cmdCtx)
	if res.Err != nil {
		t.Fatalf("LASTSAVE after save: %v", res.Err)
	}
	ts, ok := res.Value.(int64)
	if !ok {
		t.Fatalf("LASTSAVE value type = %T, want int64", res.Value)
	}
	if ts < before || ts > after {
		t.Errorf("LASTSAVE = %d, want in [%d, %d]", ts, before, after)
	}
}

func TestHandler_Lastsave_NoSnapshotter(t *testing.T) {
	_, e, ctx := setup(t)

	cmdCtx := &command.Context{
		Client: ctx,
		Op:     "LASTSAVE",
		Engine: e,
		Cache:  cache.New(),
	}
	res := handler.HandleLastsave(cmdCtx)
	if res.Err == nil {
		t.Fatal("LASTSAVE with nil snapshotter: expected error")
	}
}
