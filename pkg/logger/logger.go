package logger

import (
	"os"

	"github.com/rs/zerolog"
)

var log zerolog.Logger

func Init(level string) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	log = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Logger().Level(lvl)
}

func Trace() *zerolog.Event { return log.Trace() }
func Debug() *zerolog.Event { return log.Debug() }
func Info() *zerolog.Event  { return log.Info() }
func Warn() *zerolog.Event  { return log.Warn() }
func Error() *zerolog.Event { return log.Error() }
func Fatal() *zerolog.Event { return log.Fatal() }
