package clientctx

import "testing"

func TestNew(t *testing.T) {
	c := New()
	if c.ProtoVersion != 2 {
		t.Errorf("expected ProtoVersion 2, got %d", c.ProtoVersion)
	}
	if c.InTransaction {
		t.Error("expected InTransaction false")
	}
	if c.Authenticated {
		t.Error("expected Authenticated false")
	}
	if c.WatchDirty {
		t.Error("expected WatchDirty false")
	}
	if len(c.WatchedKeys) != 0 {
		t.Error("expected empty WatchedKeys")
	}
	if len(c.CommandQueue) != 0 {
		t.Error("expected empty CommandQueue")
	}
}

func TestTransactionLifecycle(t *testing.T) {
	c := New()

	c.StartTransaction()
	if !c.InTransaction {
		t.Error("expected InTransaction true after StartTransaction")
	}

	c.EnqueueCommand([]string{"SET", "a", "1"})
	c.EnqueueCommand([]string{"GET", "a"})
	if len(c.CommandQueue) != 2 {
		t.Errorf("expected 2 queued commands, got %d", len(c.CommandQueue))
	}

	c.ResetTransaction()
	if c.InTransaction {
		t.Error("expected InTransaction false after ResetTransaction")
	}
	if c.CommandQueue != nil {
		t.Error("expected nil CommandQueue after ResetTransaction")
	}
}

func TestClearWatch(t *testing.T) {
	c := New()
	c.WatchedKeys["foo"] = struct{}{}
	c.WatchedKeys["bar"] = struct{}{}
	c.WatchDirty = true

	c.ClearWatch()
	if c.WatchDirty {
		t.Error("expected WatchDirty false after ClearWatch")
	}
	if len(c.WatchedKeys) != 0 {
		t.Error("expected empty WatchedKeys after ClearWatch")
	}
}
