package workers

import (
	"context"
	"gocache/pkg/cache"
	"gocache/pkg/engine"
	"gocache/pkg/persistence"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setup(t *testing.T) (*cache.Cache, *engine.Engine) {
	t.Helper()
	c := cache.New()
	e := engine.New(c)
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

	gob := persistence.NewGobSource(file)
	coord := persistence.New(gob)
	coord.SetStore(c)
	coord.RegisterSnapshotter(gob)

	w := NewSnapshotWorker(c, e, 50*time.Millisecond)
	w.SetSnapshotInvoker(coord)
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

// Hot-reload of the snapshot file path is now the plugin's
// responsibility — it implements apiconfig.ReloadHandler and the
// server routes reload events via pkg/config.OnPluginReload (see
// plugins/snapshot/init.go). The worker no longer holds the filename,
// so there's no SnapshotWorker-side hot-reload path to test here.

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
