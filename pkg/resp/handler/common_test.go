package handler_test

import (
	"strings"
	"testing"

	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/engine"
	"gocache/pkg/resp/handler"
	"gocache/pkg/transaction"
)

// Build a plain handler map from registrations for test dispatch.
var handlers = func() map[string]command.Handler {
	m := make(map[string]command.Handler)
	for name, reg := range handler.Registrations() {
		m[name] = reg.Handler
	}
	return m
}()

func setup(t *testing.T) (*cache.Cache, *engine.Engine, *clientctx.ClientContext) {
	t.Helper()
	c := cache.New()
	e := engine.New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })
	ctx := clientctx.New()
	return c, e, ctx
}

// setupCtx returns only a ClientContext for tests that create their own cache/engine.
func setupCtx(t *testing.T) *clientctx.ClientContext {
	t.Helper()
	return clientctx.New()
}

// valueAsString coerces a command result into a string. GET-like handlers
// return []byte; production callers convert via mapValueToResp, but
// handler-level tests dispatch directly. The helper papers over the
// boundary so test assertions can stay in terms of the user-visible string.
func valueAsString(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return ""
	}
}

// eval dispatches a command through the handler map, mimicking the evaluator
// pipeline for test convenience.
func eval(t *testing.T, c *cache.Cache, e *engine.Engine, ctx *clientctx.ClientContext, op string, args []string) command.Result {
	t.Helper()
	op = strings.ToUpper(op)
	h, ok := handlers[op]
	if !ok {
		t.Fatalf("unknown command: %s", op)
	}
	cmdCtx := &command.Context{
		Client:      ctx,
		Op:          op,
		Args:        args,
		Engine:      e,
		Cache:       c,
		Transaction: transaction.NewManager(),
	}
	return h(cmdCtx)
}
