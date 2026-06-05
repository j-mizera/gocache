// Package logger provides a structured logger for GoCache server and plugins.
//
// Both server and plugin code import this package. Every log line is JSON
// written to a configurable io.Writer (stdout by default). The log collector
// worker reads from the pipe and emits periodic runtime log batches to the event bus.
//
// The default log methods (Trace, Debug, Info, Warn, Error) accept a
// context.Context for call-site compatibility, but do not derive operation
// correlation from it. Correlated telemetry must be submitted through an
// explicit operation scope after the operation has been created.
//
// Use the NoCtx variants at boundaries that intentionally emit local zerolog
// records without telemetry materialization, such as early startup before the
// bootstrap operation, plugin discovery/loading, and config parsing.
package logger

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"time"

	apiobs "gocache/api/observability"

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

// --- Default log methods (context accepted, no operation extraction) ---
// These methods keep the historical context-taking API, but context is
// cancellation/request plumbing only. Operation-correlated logs must be
// recorded through explicit observability scopes before local materialization.

func (l *Logger) Trace(context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Trace()}
}
func (l *Logger) Debug(context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Debug()}
}
func (l *Logger) Info(context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Info()}
}
func (l *Logger) Warn(context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Warn()}
}
func (l *Logger) Error(context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Error()}
}

// Fatal logs at fatal level, then exits the process after the message is
// written. The os.Exit(1) happens inside zerolog's Msg/Msgf call — Msg MUST
// be invoked for the exit to occur.
func (l *Logger) Fatal(context.Context) *OpEvent {
	return &OpEvent{event: l.zl.Fatal()}
}

// --- NoCtx methods (WITHOUT operation context) ---
// For boundaries with no operation: early startup, plugin loading, config loading.

func (l *Logger) TraceNoCtx() *zerolog.Event { return l.zl.Trace() }
func (l *Logger) DebugNoCtx() *zerolog.Event { return l.zl.Debug() }
func (l *Logger) InfoNoCtx() *zerolog.Event  { return l.zl.Info() }
func (l *Logger) WarnNoCtx() *zerolog.Event  { return l.zl.Warn() }
func (l *Logger) ErrorNoCtx() *zerolog.Event { return l.zl.Error() }
func (l *Logger) FatalNoCtx() *zerolog.Event { return l.zl.Fatal() }

// TelemetryNoCtx starts a local materialization event for a telemetry log level.
// It deliberately uses zerolog.WithLevel so fatal/panic telemetry records can be
// written before the caller finishes telemetry and decides whether to exit/panic.
func (l *Logger) TelemetryNoCtx(level apiobs.TelemetryLogLevel) *zerolog.Event {
	return l.zl.WithLevel(telemetryLevelToZerolog(level))
}

func telemetryLevelToZerolog(level apiobs.TelemetryLogLevel) zerolog.Level {
	switch level {
	case apiobs.TelemetryLogLevelTrace:
		return zerolog.TraceLevel
	case apiobs.TelemetryLogLevelDebug:
		return zerolog.DebugLevel
	case apiobs.TelemetryLogLevelInfo:
		return zerolog.InfoLevel
	case apiobs.TelemetryLogLevelWarn:
		return zerolog.WarnLevel
	case apiobs.TelemetryLogLevelError:
		return zerolog.ErrorLevel
	case apiobs.TelemetryLogLevelFatal:
		return zerolog.FatalLevel
	case apiobs.TelemetryLogLevelPanic:
		return zerolog.PanicLevel
	default:
		return zerolog.NoLevel
	}
}

// OpEvent wraps a zerolog.Event while preserving the historical fluent API.
// It deliberately does not derive operation correlation from context; callers
// that need correlated output must record through an explicit operation scope.
type OpEvent struct {
	event *zerolog.Event
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
	e.event.Msg(msg)
}

func (e *OpEvent) Msgf(format string, args ...any) {
	e.event.Msgf(format, args...)
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
func Fatal(ctx context.Context) *OpEvent { return Default().Fatal(ctx) }

// --- Package-level convenience functions (NO context, delegate to Default()) ---

func TraceNoCtx() *zerolog.Event { return Default().TraceNoCtx() }
func DebugNoCtx() *zerolog.Event { return Default().DebugNoCtx() }
func InfoNoCtx() *zerolog.Event  { return Default().InfoNoCtx() }
func WarnNoCtx() *zerolog.Event  { return Default().WarnNoCtx() }
func ErrorNoCtx() *zerolog.Event { return Default().ErrorNoCtx() }
func FatalNoCtx() *zerolog.Event { return Default().FatalNoCtx() }

func TelemetryNoCtx(level apiobs.TelemetryLogLevel) *zerolog.Event {
	return Default().TelemetryNoCtx(level)
}
