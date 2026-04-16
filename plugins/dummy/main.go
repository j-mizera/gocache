package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	apilogger "gocache/api/logger"
	"gocache/sdk/pluginsdk"
)

type dummyPlugin struct {
	log *apilogger.Logger
}

func (d *dummyPlugin) Name() string    { return "dummy" }
func (d *dummyPlugin) Version() string { return "0.1.0" }
func (d *dummyPlugin) Critical() bool  { return false }

func (d *dummyPlugin) OnHealthCheck(_ context.Context) error {
	return nil
}

func (d *dummyPlugin) OnShutdown(_ context.Context) error {
	d.log.InfoNoCtx().Msg("shutting down")
	return nil
}

func main() {
	plog := apilogger.New(os.Stdout, "dummy", "debug")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := pluginsdk.Run(ctx, &dummyPlugin{log: plog}); err != nil {
		plog.ErrorNoCtx().Err(err).Msg("plugin error")
		os.Exit(1)
	}
}
