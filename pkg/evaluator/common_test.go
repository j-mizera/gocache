package evaluator_test

import (
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/evaluator"
	"testing"
)

func setup(t *testing.T) (*cache.Cache, *engine.Engine, evaluator.Evaluator, *clientctx.ClientContext) {
	t.Helper()
	c := cache.New()
	e := engine.New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })
	ev := evaluator.New(c, e, "", "", nil, nil)
	ctx := clientctx.New()
	return c, e, ev, ctx
}
