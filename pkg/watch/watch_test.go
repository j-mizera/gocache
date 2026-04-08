package watch

import (
	"gocache/pkg/clientctx"
	"testing"
)

func TestWatch(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	m.Watch(ctx, []string{"key1", "key2"})

	if _, ok := ctx.WatchedKeys["key1"]; !ok {
		t.Error("expected key1 in WatchedKeys")
	}
	if _, ok := ctx.WatchedKeys["key2"]; !ok {
		t.Error("expected key2 in WatchedKeys")
	}
}

func TestUnwatch(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	m.Watch(ctx, []string{"key1", "key2"})
	m.Unwatch(ctx)

	if len(ctx.WatchedKeys) != 0 {
		t.Error("expected empty WatchedKeys after Unwatch")
	}
	if ctx.WatchDirty {
		t.Error("expected WatchDirty false after Unwatch")
	}
}

func TestNotifyMutation(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	m.Watch(ctx, []string{"mykey"})
	m.NotifyMutation("mykey")

	if !ctx.WatchDirty {
		t.Error("expected WatchDirty true after NotifyMutation")
	}
}

func TestNotifyMutation_UnrelatedKey(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	m.Watch(ctx, []string{"mykey"})
	m.NotifyMutation("otherkey")

	if ctx.WatchDirty {
		t.Error("expected WatchDirty false for unrelated key mutation")
	}
}

func TestNotifyAll(t *testing.T) {
	m := NewManager()
	ctx1 := clientctx.New()
	ctx2 := clientctx.New()

	m.Watch(ctx1, []string{"a"})
	m.Watch(ctx2, []string{"b"})

	m.NotifyAll()

	if !ctx1.WatchDirty {
		t.Error("expected ctx1 dirty after NotifyAll")
	}
	if !ctx2.WatchDirty {
		t.Error("expected ctx2 dirty after NotifyAll")
	}
}

func TestMultipleWatchersOnSameKey(t *testing.T) {
	m := NewManager()
	ctx1 := clientctx.New()
	ctx2 := clientctx.New()

	m.Watch(ctx1, []string{"shared"})
	m.Watch(ctx2, []string{"shared"})

	m.NotifyMutation("shared")

	if !ctx1.WatchDirty {
		t.Error("expected ctx1 dirty")
	}
	if !ctx2.WatchDirty {
		t.Error("expected ctx2 dirty")
	}
}

func TestUnwatch_CleansUpWatcherMap(t *testing.T) {
	m := NewManager()
	ctx := clientctx.New()

	m.Watch(ctx, []string{"k1", "k2"})
	m.Unwatch(ctx)

	// After unwatch, mutations should not affect ctx.
	m.NotifyMutation("k1")
	if ctx.WatchDirty {
		t.Error("expected no notification after Unwatch")
	}
}
