// Package logger provides a structured logger for GoCache server and plugins.
//
// Both server and plugin code import this package. Every log line is JSON
// written to a configurable io.Writer (stdout by default). The log collector
// worker reads from the pipe and emits LogEntry events to the event bus.
//
// The Logger carries a source tag (e.g. "server", "gobservability") and
// supports operation-scoped logging via Op* methods that include the
// operation ID and redacted operation context in the JSON output.
package logger

import (
	"io"
	"os"
	"sync/atomic"

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

// --- Standard log methods (no operation context) ---

func (l *Logger) Trace() *zerolog.Event { return l.zl.Trace() }
func (l *Logger) Debug() *zerolog.Event { return l.zl.Debug() }
func (l *Logger) Info() *zerolog.Event  { return l.zl.Info() }
func (l *Logger) Warn() *zerolog.Event  { return l.zl.Warn() }
func (l *Logger) Error() *zerolog.Event { return l.zl.Error() }
func (l *Logger) Fatal() *zerolog.Event { return l.zl.Fatal() }

// --- Operation-scoped log methods ---

// OpTrace logs at Trace level with operation context.
func (l *Logger) OpTrace(op *ops.Operation) *OpEvent {
	return &OpEvent{event: l.zl.Trace(), op: op}
}

// OpDebug logs at Debug level with operation context.
func (l *Logger) OpDebug(op *ops.Operation) *OpEvent {
	return &OpEvent{event: l.zl.Debug(), op: op}
}

// OpInfo logs at Info level with operation context.
func (l *Logger) OpInfo(op *ops.Operation) *OpEvent {
	return &OpEvent{event: l.zl.Info(), op: op}
}

// OpWarn logs at Warn level with operation context.
func (l *Logger) OpWarn(op *ops.Operation) *OpEvent {
	return &OpEvent{event: l.zl.Warn(), op: op}
}

// OpError logs at Error level with operation context.
func (l *Logger) OpError(op *ops.Operation) *OpEvent {
	return &OpEvent{event: l.zl.Error(), op: op}
}

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

var (
	defaultLogger atomic.Pointer[Logger]
)

// Init initializes the default server logger writing to stdout.
func Init(level string) {
	defaultLogger.Store(New(os.Stdout, "server", level))
}

// Default returns the default logger. Thread-safe.
func Default() *Logger {
	if l := defaultLogger.Load(); l != nil {
		return l
	}
	// Lazy init for tests that don't call Init().
	l := New(os.Stdout, "server", "info")
	defaultLogger.CompareAndSwap(nil, l)
	return defaultLogger.Load()
}

// --- Package-level convenience functions (delegate to Default()) ---

func Trace() *zerolog.Event { return Default().Trace() }
func Debug() *zerolog.Event { return Default().Debug() }
func Info() *zerolog.Event  { return Default().Info() }
func Warn() *zerolog.Event  { return Default().Warn() }
func Error() *zerolog.Event { return Default().Error() }
func Fatal() *zerolog.Event { return Default().Fatal() }
