package workers

import (
	"context"
	"gocache/pkg/cache"
	"gocache/pkg/engine"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setup(t *testing.T) (*cache.Cache, *engine.Engine) {
	t.Helper()
	c := cache.New()
	e := engine.New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })
	return c, e
}

func TestSnapshotWorker_CreatesFile(t *testing.T) {
	c, e := setup(t)

	// Add some data.
	e.Dispatch(context.Background(), func() {
		_ = c.RawSet(context.Background(), "key", "val", 0)
	})

	dir := t.TempDir()
	file := filepath.Join(dir, "test_snapshot.dat")

	w := NewSnapshotWorker(c, e, 50*time.Millisecond, file)
	w.Start(context.Background())
	defer w.Stop()

	// Wait for at least one tick.
	time.Sleep(200 * time.Millisecond)

	if _, err := os.Stat(file); os.IsNotExist(err) {
		t.Error("snapshot file was not created")
	}
}

func TestCleanupWorker_RemovesExpired(t *testing.T) {
	c, e := setup(t)

	// Add an already-expired key.
	e.Dispatch(context.Background(), func() {
		_ = c.RawSet(context.Background(), "expired", "val", time.Now().Add(-time.Hour).UnixNano())
		_ = c.RawSet(context.Background(), "alive", "val", 0)
	})

	w := NewCleanupWorker(c, e, 50*time.Millisecond)
	w.Start(context.Background())
	defer w.Stop()

	time.Sleep(200 * time.Millisecond)

	res, err := e.DispatchWithResult(context.Background(), func() any {
		_, found := c.RawGet("expired")
		return found
	})
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if res.(bool) {
		t.Error("expected expired key to be cleaned up")
	}

	res, err = e.DispatchWithResult(context.Background(), func() any {
		_, found := c.RawGet("alive")
		return found
	})
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if !res.(bool) {
		t.Error("expected alive key to remain")
	}
}

func TestWorker_Stop(t *testing.T) {
	_, e := setup(t)

	w := NewCleanupWorker(cache.New(), e, time.Hour)
	w.Start(context.Background())
	w.Stop()
	w.Stop() // idempotent
}

func TestWorker_UpdateInterval(t *testing.T) {
	_, e := setup(t)

	w := NewCleanupWorker(cache.New(), e, time.Hour)
	w.Start(context.Background())
	defer w.Stop()

	// Should not block or panic.
	w.UpdateInterval(50 * time.Millisecond)
}

func TestSnapshotWorker_UpdateFile(t *testing.T) {
	c, e := setup(t)
	e.Dispatch(context.Background(), func() {
		_ = c.RawSet(context.Background(), "k", "v", 0)
	})

	dir := t.TempDir()
	file1 := filepath.Join(dir, "snap1.dat")
	file2 := filepath.Join(dir, "snap2.dat")

	w := NewSnapshotWorker(c, e, 50*time.Millisecond, file1)
	w.Start(context.Background())

	// Switch to file2.
	w.UpdateFile(file2)
	time.Sleep(200 * time.Millisecond)

	// Stop before TempDir cleanup to avoid race on directory removal.
	w.Stop()

	if _, err := os.Stat(file2); os.IsNotExist(err) {
		t.Error("expected snapshot at updated file path")
	}
}

func TestSafeInterval_ZeroDefault(t *testing.T) {
	d := safeInterval(0)
	if d != defaultInterval {
		t.Errorf("expected default interval, got %v", d)
	}
	d = safeInterval(-1 * time.Second)
	if d != defaultInterval {
		t.Errorf("expected default interval for negative, got %v", d)
	}
}
