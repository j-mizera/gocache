package command

import (
	"context"
	"testing"

	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/transaction"
	"gocache/pkg/watch"
)

// TestContext_Reset verifies that every field is zeroed so the value can
// be recycled through a sync.Pool without dragging stale references along.
// New fields added to *Context must be reset here too — otherwise the pool
// silently leaks pointers across calls.
func TestContext_Reset(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })

	ctx := &Context{
		Client:           clientctx.New(),
		Op:               "SET",
		Args:             []string{"k", "v"},
		InBatch:          true,
		Engine:           e,
		Cache:            c,
		Transaction:      transaction.NewManager(),
		BlockingRegistry: blocking.NewRegistry(),
		WatchManager:     watch.NewManager(),
		RequirePass:      "secret",
		EvalFn: func(_ context.Context, _ *clientctx.ClientContext, _ string, _ []string, _ bool) Result {
			return Result{}
		},
	}
	type testKey struct{}
	ctx.SetContext(context.WithValue(context.Background(), testKey{}, "x"))

	ctx.Reset()

	zeros := []struct {
		name string
		zero bool
	}{
		{"ctx", ctx.Context() == context.Background()}, // Context() returns Background when nil
		{"Client", ctx.Client == nil},
		{"Op", ctx.Op == ""},
		{"Args", ctx.Args == nil},
		{"InBatch", ctx.InBatch == false},
		{"Engine", ctx.Engine == nil},
		{"Cache", ctx.Cache == nil},
		{"Transaction", ctx.Transaction == nil},
		{"BlockingRegistry", ctx.BlockingRegistry == nil},
		{"WatchManager", ctx.WatchManager == nil},
		{"RequirePass", ctx.RequirePass == ""},
		{"EvalFn", ctx.EvalFn == nil},
	}
	for _, f := range zeros {
		if !f.zero {
			t.Errorf("Reset left %s non-zero", f.name)
		}
	}
}
