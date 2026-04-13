// Package command provides server-side command types for GoCache.
//
// Shared types (Result, Spec) are re-exported from api/command.
// This package adds server-only types: Handler, Context, Dispatch, Registration.
package command

import (
	apicommand "gocache/api/command"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/clientctx"
	"gocache/pkg/engine"
	"gocache/pkg/transaction"
	"gocache/pkg/watch"
)

// Re-export shared types from api/command so existing importers don't break.
type Result = apicommand.Result
type Spec = apicommand.Spec

// Handler is a function that handles a single cache command.
type Handler func(ctx *Context) Result

// Context carries all dependencies needed to execute a command.
type Context struct {
	Client           *clientctx.ClientContext
	Op               string
	Args             []string
	InBatch          bool
	Engine           *engine.Engine
	Cache            *cache.Cache
	Transaction      *transaction.Manager
	BlockingRegistry *blocking.Registry
	WatchManager     *watch.Manager

	// Evaluator-level config, set by the pipeline before dispatch.
	SnapshotFile string
	RequirePass  string

	// EvalFn re-enters the evaluator pipeline. Used by EXEC to execute
	// queued commands in a batch. The bool parameter is inBatch.
	EvalFn func(ctx *clientctx.ClientContext, op string, args []string, inBatch bool) Result
}

// Dispatch runs fn either directly (when InBatch is true, meaning the engine
// lock is already held) or through the engine dispatcher. It wraps the result
// into a Result, propagating any error.
func Dispatch(ctx *Context, fn func() interface{}) Result {
	var res interface{}
	if ctx.InBatch {
		res = fn()
	} else {
		res = ctx.Engine.DispatchWithResult(fn)
	}
	if err, ok := res.(error); ok {
		return Result{Err: err}
	}
	return Result{Value: res}
}

// Registration bundles a command handler with its argument spec.
// Handler packages return maps of these so the evaluator pipeline can
// validate args without hardcoding spec knowledge.
type Registration struct {
	Handler Handler
	Spec    Spec
}
