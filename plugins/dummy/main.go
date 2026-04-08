package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gocache/pkg/pluginsdk"
)

type dummyPlugin struct{}

func (d *dummyPlugin) Name() string    { return "dummy" }
func (d *dummyPlugin) Version() string { return "0.1.0" }
func (d *dummyPlugin) Critical() bool  { return false }

func (d *dummyPlugin) OnHealthCheck(_ context.Context) error {
	return nil
}

func (d *dummyPlugin) OnShutdown(_ context.Context) error {
	log.Println("dummy plugin shutting down")
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := pluginsdk.Run(ctx, &dummyPlugin{}); err != nil {
		log.Printf("dummy plugin error: %v", err)
		os.Exit(1)
	}
}
