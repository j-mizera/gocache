// Package command provides server-side command types for GoCache.
//
// Shared types (Result, Spec) are re-exported from api/command.
// This package adds server-only types: Handler, Context, Dispatch, Registration.
package command

import (
	"context"

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
//
// Context is request-scoped and short-lived: it is constructed per command
// by the evaluator and discarded when the handler returns. The ambient
// context.Context is held in an unexported field and exposed via the
// Context() method, following the http.Request precedent — see
// (*Context).Context and (*Context).SetContext.
type Context struct {
	ctx              context.Context
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
	// queued commands in a batch. parentCtx is the connection-scoped ctx
	// from the outer Evaluate call.
	EvalFn func(parentCtx context.Context, client *clientctx.ClientContext, op string, args []string, inBatch bool) Result
}

// Context returns the ambient context.Context carrying the current
// *ops.Operation (retrievable via operations.FromContext). Handlers
// should pass this down to cache/persistence calls and logger calls so
// logs stay correlated with the command operation.
//
// Do NOT capture the returned context in a goroutine that outlives the
// handler call: the command operation completes when the handler returns,
// and a later log would carry a stale (completed) operation.
//
// Returns context.Background() if no context was set, so callers never
// receive nil.
func (c *Context) Context() context.Context {
	if c.ctx == nil {
		return context.Background()
	}
	return c.ctx
}

// SetContext assigns the ambient context.Context. Called by the evaluator
// when building the Context before handler dispatch.
func (c *Context) SetContext(ctx context.Context) {
	c.ctx = ctx
}

// Dispatch runs fn either directly (when InBatch is true, meaning the engine
// lock is already held) or through the engine dispatcher. It wraps the result
// into a Result, propagating any error. If the engine is stopped or the
// command context is cancelled before fn runs, the returned Result carries
// that error.
func Dispatch(ctx *Context, fn func() any) Result {
	if ctx.InBatch {
		res := fn()
		if err, ok := res.(error); ok {
			return Result{Err: err}
		}
		return Result{Value: res}
	}
	res, err := ctx.Engine.DispatchWithResult(ctx.Context(), fn)
	if err != nil {
		return Result{Err: err}
	}
	if resultErr, ok := res.(error); ok {
		return Result{Err: resultErr}
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
