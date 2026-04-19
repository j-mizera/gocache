// Package logger provides a structured logger for GoCache server and plugins.
//
// Both server and plugin code import this package. Every log line is JSON
// written to a configurable io.Writer (stdout by default). The log collector
// worker reads from the pipe and emits LogEntry events to the event bus.
//
// The default log methods (Trace, Debug, Info, Warn, Error) take a
// context.Context and extract the current *ops.Operation via
// operations.FromContext. If no operation is present the log line is still
// written but without operation correlation.
//
// Use the NoCtx variants only at boundaries where no operation exists:
// early startup before the bootstrap operation, plugin discovery/loading,
// and config parsing.
package logger

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"time"

	"gocache/api/command"
	ops "gocache/api/operations"

	"github.com/rs/zerolog"
)

// Logger is a structured logger with a source identifier.
type Logger struct {
	zl zerolog.Logger
}

// New creates a Logger that writes JSON to w with the given source tag.
func New(w io.Writer, source, level string) *Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	zl := zerolog.New(zerolog.SyncWriter(w)).With().Timestamp().Str("source", source).Logger().Level(lvl)
	return &Logger{zl: zl}
}

// --- Default log methods (WITH operation context via ctx) ---
// These are the standard methods. Every log call should pass the ambient ctx.
// The operation is extracted via ops.FromContext(ctx); if nil, context is omitted.

func (l *Logger) Trace(ctx context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Trace(), op: ops.FromContext(ctx)}
}
func (l *Logger) Debug(ctx context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Debug(), op: ops.FromContext(ctx)}
}
func (l *Logger) Info(ctx context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Info(), op: ops.FromContext(ctx)}
}
func (l *Logger) Warn(ctx context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Warn(), op: ops.FromContext(ctx)}
}
func (l *Logger) Error(ctx context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Error(), op: ops.FromContext(ctx)}
}

// --- NoCtx methods (WITHOUT operation context) ---
// For boundaries with no operation: early startup, plugin loading, config loading.

func (l *Logger) TraceNoCtx() *zerolog.Event { return l.zl.Trace() }
func (l *Logger) DebugNoCtx() *zerolog.Event { return l.zl.Debug() }
func (l *Logger) InfoNoCtx() *zerolog.Event  { return l.zl.Info() }
func (l *Logger) WarnNoCtx() *zerolog.Event  { return l.zl.Warn() }
func (l *Logger) ErrorNoCtx() *zerolog.Event { return l.zl.Error() }
func (l *Logger) FatalNoCtx() *zerolog.Event { return l.zl.Fatal() }

// OpEvent wraps a zerolog.Event with an optional operation.
// On Msg/Msgf, the operation's ID and redacted context are injected into JSON.
type OpEvent struct {
	event *zerolog.Event
	op    *ops.Operation
}

func (e *OpEvent) Str(key, val string) *OpEvent {
	e.event = e.event.Str(key, val)
	return e
}

func (e *OpEvent) Int(key string, val int) *OpEvent {
	e.event = e.event.Int(key, val)
	return e
}

func (e *OpEvent) Int64(key string, val int64) *OpEvent {
	e.event = e.event.Int64(key, val)
	return e
}

func (e *OpEvent) Err(err error) *OpEvent {
	e.event = e.event.Err(err)
	return e
}

func (e *OpEvent) Bool(key string, val bool) *OpEvent {
	e.event = e.event.Bool(key, val)
	return e
}

func (e *OpEvent) Strs(key string, vals []string) *OpEvent {
	e.event = e.event.Strs(key, vals)
	return e
}

func (e *OpEvent) Dur(key string, val time.Duration) *OpEvent {
	e.event = e.event.Dur(key, val)
	return e
}

func (e *OpEvent) Interface(key string, val any) *OpEvent {
	e.event = e.event.Interface(key, val)
	return e
}

func (e *OpEvent) Msg(msg string) {
	e.injectContext()
	e.event.Msg(msg)
}

func (e *OpEvent) Msgf(format string, args ...any) {
	e.injectContext()
	e.event.Msgf(format, args...)
}

func (e *OpEvent) injectContext() {
	if e.op == nil {
		return
	}
	e.event = e.event.Str(command.OperationID, e.op.ID)
	ctx := e.op.ContextSnapshot(true) // redacted — secrets stripped
	if len(ctx) > 0 {
		e.event = e.event.Interface(command.CtxField, ctx)
	}
}

// --- Default logger (server uses this, writes to stdout) ---

var defaultLogger atomic.Pointer[Logger]

// Init initializes the default server logger writing to stdout.
func Init(level string) {
	defaultLogger.Store(New(os.Stdout, "server", level))
}

// InitWithWriter initializes the default server logger writing to a custom writer.
// Used by main.go to pipe logs through the log collector while teeing to stderr.
func InitWithWriter(w io.Writer, level string) {
	defaultLogger.Store(New(w, "server", level))
}

// Default returns the default logger. Thread-safe.
func Default() *Logger {
	if l := defaultLogger.Load(); l != nil {
		return l
	}
	l := New(os.Stdout, "server", "info")
	defaultLogger.CompareAndSwap(nil, l)
	return defaultLogger.Load()
}

// --- Package-level convenience functions (WITH context, delegate to Default()) ---

func Trace(ctx context.Context) *OpEvent { return Default().Trace(ctx) }
func Debug(ctx context.Context) *OpEvent { return Default().Debug(ctx) }
func Info(ctx context.Context) *OpEvent  { return Default().Info(ctx) }
func Warn(ctx context.Context) *OpEvent  { return Default().Warn(ctx) }
func Error(ctx context.Context) *OpEvent { return Default().Error(ctx) }

// --- Package-level convenience functions (NO context, delegate to Default()) ---

func TraceNoCtx() *zerolog.Event { return Default().TraceNoCtx() }
func DebugNoCtx() *zerolog.Event { return Default().DebugNoCtx() }
func InfoNoCtx() *zerolog.Event  { return Default().InfoNoCtx() }
func WarnNoCtx() *zerolog.Event  { return Default().WarnNoCtx() }
func ErrorNoCtx() *zerolog.Event { return Default().ErrorNoCtx() }
func FatalNoCtx() *zerolog.Event { return Default().FatalNoCtx() }
