package evaluator_test

import (
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/evaluator"
	"os"
	"testing"
)

func TestEvaluator_Snapshot(t *testing.T) {
	snapshotFile := "test_eval_snapshot.dat"
	filename := "test_eval_snapshot_2.dat"
	defer os.Remove(snapshotFile)
	defer os.Remove(filename)

	cacheInstance1 := cache.New()
	engineInstance1 := engine.New(cacheInstance1)
	go engineInstance1.Run()
	ev := evaluator.New(cacheInstance1, engineInstance1, snapshotFile, "", nil, nil)
	ctx := clientctx.New()

	ev.Evaluate(ctx, "SET", []string{"snap", "data"})

	res := ev.Evaluate(ctx, "SNAPSHOT", nil)
	if res.Value != "OK" {
		t.Fatalf("failed to save snapshot: %v", res.Value)
	}

	// Verify file exists
	if _, err := os.Stat(snapshotFile); os.IsNotExist(err) {
		t.Fatalf("%s was not created", snapshotFile)
	}
	// Move it to our test filename
	os.Rename(snapshotFile, filename)

	cacheInstance2 := cache.New()
	engineInstance2 := engine.New(cacheInstance2)
	go engineInstance2.Run()
	evaluatorInstance2 := evaluator.New(cacheInstance2, engineInstance2, "", "", nil, nil)
	ctx2 := clientctx.New()

	res = evaluatorInstance2.Evaluate(ctx2, "LOAD_SNAPSHOT", []string{filename})
	if res.Value != "OK" {
		t.Fatalf("failed to load snapshot: %v", res.Value)
	}

	res = evaluatorInstance2.Evaluate(ctx2, "GET", []string{"snap"})
	if res.Value != "data" {
		t.Errorf("expected data, got %v", res.Value)
	}
}
