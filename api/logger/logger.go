// Package logger provides a structured logger for GoCache server and plugins.
//
// Both server and plugin code import this package. Every log line is JSON
// written to a configurable io.Writer (stdout by default). The log collector
// worker reads from the pipe and emits LogEntry events to the event bus.
//
// The default log methods (Trace, Debug, Info, Warn, Error) require an
// operation context. Use TraceNoCtx, DebugNoCtx, etc. for the rare cases
// where no operation is active (early startup, plugin loading).
package logger

import (
	"io"
	"os"
	"sync/atomic"
	"time"

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

// --- Default log methods (WITH operation context) ---
// These are the standard methods. Every log call should pass an operation.
// The operation may be nil — context is simply omitted in that case.

func (l *Logger) Trace(op *ops.Operation) *OpEvent { return &OpEvent{event: l.zl.Trace(), op: op} }
func (l *Logger) Debug(op *ops.Operation) *OpEvent { return &OpEvent{event: l.zl.Debug(), op: op} }
func (l *Logger) Info(op *ops.Operation) *OpEvent  { return &OpEvent{event: l.zl.Info(), op: op} }
func (l *Logger) Warn(op *ops.Operation) *OpEvent  { return &OpEvent{event: l.zl.Warn(), op: op} }
func (l *Logger) Error(op *ops.Operation) *OpEvent { return &OpEvent{event: l.zl.Error(), op: op} }

// --- NoCtx methods (WITHOUT operation context) ---
// For the rare cases where no operation is active: early startup, plugin loading,
// shutdown after operations are cleaned up.

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

func (e *OpEvent) Interface(key string, val interface{}) *OpEvent {
	e.event = e.event.Interface(key, val)
	return e
}

func (e *OpEvent) Msg(msg string) {
	e.injectContext()
	e.event.Msg(msg)
}

func (e *OpEvent) Msgf(format string, args ...interface{}) {
	e.injectContext()
	e.event.Msgf(format, args...)
}

func (e *OpEvent) injectContext() {
	if e.op == nil {
		return
	}
	e.event = e.event.Str("_operation_id", e.op.ID)
	ctx := e.op.ContextSnapshot(true) // redacted — secrets stripped
	if len(ctx) > 0 {
		e.event = e.event.Interface("_ctx", ctx)
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

func Trace(op *ops.Operation) *OpEvent { return Default().Trace(op) }
func Debug(op *ops.Operation) *OpEvent { return Default().Debug(op) }
func Info(op *ops.Operation) *OpEvent  { return Default().Info(op) }
func Warn(op *ops.Operation) *OpEvent  { return Default().Warn(op) }
func Error(op *ops.Operation) *OpEvent { return Default().Error(op) }

// --- Package-level convenience functions (NO context, delegate to Default()) ---

func TraceNoCtx() *zerolog.Event { return Default().TraceNoCtx() }
func DebugNoCtx() *zerolog.Event { return Default().DebugNoCtx() }
func InfoNoCtx() *zerolog.Event  { return Default().InfoNoCtx() }
func WarnNoCtx() *zerolog.Event  { return Default().WarnNoCtx() }
func ErrorNoCtx() *zerolog.Event { return Default().ErrorNoCtx() }
func FatalNoCtx() *zerolog.Event { return Default().FatalNoCtx() }
