package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/config"
	"gocache/pkg/engine"
	"gocache/pkg/logger"
	"gocache/pkg/persistence"
	"gocache/pkg/plugin/hooks"
	pluginmgr "gocache/pkg/plugin/manager"
	"gocache/pkg/server"
	"gocache/pkg/version"
	"gocache/pkg/watch"
	"gocache/pkg/workers"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/pflag"
)

func main() {
	// Define CLI flags — all optional; they override config file and env vars
	pflag.String("config", "gocache.yaml", "path to config file (.yaml or .json)")
	pflag.String("address", "", "server listen address (overrides config)")
	pflag.Int("port", 0, "server listen port (overrides config)")
	pflag.String("snapshot-file", "", "snapshot file path (overrides config)")
	pflag.Duration("snapshot-interval", 0, "snapshot save interval (overrides config)")
	pflag.Bool("load-on-startup", true, "load snapshot on startup (overrides config)")
	pflag.Int64("max-memory-mb", 0, "max memory in MB (overrides config)")
	pflag.String("eviction-policy", "", "eviction policy: lru, random, none (overrides config)")
	pflag.Duration("cleanup-interval", 0, "cleanup worker interval (overrides config)")
	pflag.String("log-level", "", "log level: trace, debug, info, warn, error, fatal (overrides config)")
	showVersion := pflag.Bool("version", false, "print version and exit")
	pflag.Parse()

	if *showVersion {
		fmt.Println(version.Full())
		os.Exit(0)
	}

	// Load configuration: CLI flags > env vars (GOCACHE_*) > config file > defaults
	initialCfg, v, err := config.Load(pflag.CommandLine)
	if err != nil {
		// Initialize logger with default level for fatal message
		logger.Init("info")
		logger.Fatal().Err(err).Msg("failed to load configuration")
	}

	// Wrap the config in an atomic.Pointer so the fsnotify callback goroutine
	// and the main goroutine can safely swap and read it without data races.
	var cfgPtr atomic.Pointer[config.Config]
	cfgPtr.Store(initialCfg)
	cfg := initialCfg

	// Initialize structured logging
	logger.Init(cfg.Server.LogLevel)

	logger.Info().Str("version", version.String()).Msg("starting gocache server")
	if cfgFile := v.ConfigFileUsed(); cfgFile != "" {
		logger.Info().Str("file", cfgFile).Msg("config loaded")
	}
	logger.Info().Str("addr", cfg.Server.GetAddr()).Msg("listening on")

	// Initialize core components
	cacheInstance := cache.NewWithConfig(
		cfg.Memory.MaxMemoryMB,
		cache.ParseEvictionPolicy(cfg.Memory.EvictionPolicy),
	)
	engineInstance := engine.New(cacheInstance)

	// Load snapshot if configured
	if cfg.Persistence.LoadOnStartup {
		if err := persistence.LoadSnapshot(cfg.Persistence.SnapshotFile, cacheInstance); err != nil {
			logger.Warn().Err(err).Msg("failed to load snapshot")
		} else {
			logger.Info().Str("file", cfg.Persistence.SnapshotFile).Msg("snapshot loaded")
		}
	}

	// Start engine loop
	go engineInstance.Run()

	// Initialize and start workers
	snapshotWorker := workers.NewSnapshotWorker(
		cacheInstance, engineInstance,
		cfg.Persistence.SnapshotInterval,
		cfg.Persistence.SnapshotFile,
	)
	cleanupWorker := workers.NewCleanupWorker(cacheInstance, engineInstance, cfg.Workers.CleanupInterval)
	snapshotWorker.Start()
	cleanupWorker.Start()

	// Hot reload: watch config file for changes and apply live-reloadable fields.
	// The callback runs in the fsnotify goroutine, so config swaps use
	// atomic.Pointer to avoid races with the main goroutine.
	v.WatchConfig()
	v.OnConfigChange(func(e fsnotify.Event) {
		newCfg, err := config.Reload(v)
		if err != nil {
			logger.Warn().Err(err).Msg("failed to parse updated config")
			return
		}
		logger.Info().Str("file", e.Name).Msg("config reloaded")

		prev := cfgPtr.Load()
		if newCfg.Server.GetAddr() != prev.Server.GetAddr() {
			logger.Warn().Msg("server address/port changes require a restart")
		}

		snapshotWorker.UpdateInterval(newCfg.Persistence.SnapshotInterval)
		snapshotWorker.UpdateFile(newCfg.Persistence.SnapshotFile)
		cleanupWorker.UpdateInterval(newCfg.Workers.CleanupInterval)
		cacheInstance.SetMemoryLimit(
			newCfg.Memory.MaxMemoryMB,
			cache.ParseEvictionPolicy(newCfg.Memory.EvictionPolicy),
		)

		cfgPtr.Store(newCfg)
	})

	// Initialize blocking registry for BLPOP/BRPOP
	blockingRegistry := blocking.NewRegistry()

	// Initialize watch manager for WATCH/UNWATCH optimistic locking
	watchManager := watch.NewManager()

	// Wire mutation notifications from cache to watch manager
	cacheInstance.OnMutate = watchManager.NotifyMutation
	cacheInstance.OnMutateAll = watchManager.NotifyAll

	// Initialize the server
	srv := server.New(cfg.Server.GetAddr(), cacheInstance, engineInstance, cfg.Persistence.SnapshotFile, cfg.Server.RequirePass, blockingRegistry, watchManager)

	// Set up signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize plugin system (optional — disabled by default)
	var pluginManager *pluginmgr.Manager
	if cfg.Plugins.Enabled {
		pluginManager = pluginmgr.NewManager(cfg.Plugins, srv.CoreCommandNames())
		if err := pluginManager.Start(ctx); err != nil {
			logger.Fatal().Err(err).Msg("failed to start plugin manager")
		}
		srv.SetPluginRouter(pluginManager.Router())
		srv.SetHookExecutor(hooks.NewExecutor(pluginManager.HookRegistry(), cfg.Plugins.ShutdownTimeout))
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	// Start server in a goroutine
	serverErrChan := make(chan error, 1)
	go func() {
		logger.Info().Msg("server ready to accept connections")
		if err := srv.Start(ctx); err != nil && err != context.Canceled {
			serverErrChan <- err
		}
	}()

	// Wait for shutdown signal or server error
	select {
	case sig := <-sigChan:
		logger.Info().Str("signal", sig.String()).Msg("received signal")
		handleShutdown(srv, snapshotWorker, cleanupWorker, engineInstance, cacheInstance, cfgPtr.Load(), blockingRegistry, pluginManager)
	case err := <-serverErrChan:
		logger.Error().Err(err).Msg("server error")
		handleShutdown(srv, snapshotWorker, cleanupWorker, engineInstance, cacheInstance, cfgPtr.Load(), blockingRegistry, pluginManager)
		os.Exit(1)
	}
}

func handleShutdown(
	srv *server.Server,
	snapshotWorker workers.Worker,
	cleanupWorker workers.Worker,
	engineInstance *engine.Engine,
	cacheInstance *cache.Cache,
	cfg *config.Config,
	blockingRegistry *blocking.Registry,
	pluginManager *pluginmgr.Manager,
) {
	logger.Info().Msg("starting graceful shutdown sequence")

	// Unblock all waiting BLPOP/BRPOP clients first so their connections can close.
	blockingRegistry.Shutdown()

	shutdownTimeout := 10 * time.Second
	logger.Info().Str("step", "1/6").Dur("timeout", shutdownTimeout).Msg("shutting down server")
	if err := srv.Shutdown(shutdownTimeout); err != nil {
		logger.Warn().Err(err).Msg("server shutdown error")
	}

	// Shutdown plugins before workers so plugin hooks can still fire.
	if pluginManager != nil {
		logger.Info().Str("step", "2/6").Msg("shutting down plugins")
		pluginManager.Shutdown(cfg.Plugins.ShutdownTimeout)
	}

	logger.Info().Str("step", "3/6").Msg("stopping background workers")
	snapshotWorker.Stop()
	cleanupWorker.Stop()

	logger.Info().Str("step", "4/6").Str("file", cfg.Persistence.SnapshotFile).Msg("saving final snapshot")
	if err := persistence.SaveSnapshot(cfg.Persistence.SnapshotFile, cacheInstance); err != nil {
		logger.Warn().Err(err).Msg("failed to save final snapshot")
	} else {
		logger.Info().Msg("final snapshot saved successfully")
	}

	logger.Info().Str("step", "5/6").Msg("stopping engine")
	engineInstance.Stop()

	logger.Info().Str("step", "6/6").Msg("shutdown complete")
	logger.Info().Msg("gocache server stopped gracefully")
}
