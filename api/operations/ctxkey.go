package operations

import "context"

// ctxKey is an unexported type used as a context key to prevent collisions
// with other packages. The zero-value of the struct is used as the key.
type ctxKey struct{}

// WithContext returns a child context that carries op. Call at each operation
// boundary so downstream code (cache, engine, persistence, workers) can pull
// the current operation via FromContext for logging and correlation.
func WithContext(ctx context.Context, op *Operation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKey{}, op)
}

// FromContext returns the operation stored in ctx, or nil if none is present.
// Returning nil is expected for pre-operation boundaries (startup before the
// bootstrap op, plugin loading, config loading).
func FromContext(ctx context.Context) *Operation {
	if ctx == nil {
		return nil
	}
	op, _ := ctx.Value(ctxKey{}).(*Operation)
	return op
}
