package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"gocache/api/events"
	"gocache/api/logger"
	ops "gocache/api/operations"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/config"
	"gocache/pkg/engine"
	serverEvents "gocache/pkg/events"
	"gocache/pkg/logcollector"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/persistence"
	"gocache/pkg/plugin/cmdhooks"
	pluginmgr "gocache/pkg/plugin/manager"
	"gocache/pkg/plugin/ophooks"
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
		logger.FatalNoCtx().Err(err).Msg("failed to load configuration")
	}

	// Wrap the config in an atomic.Pointer so the fsnotify callback goroutine
	// and the main goroutine can safely swap and read it without data races.
	var cfgPtr atomic.Pointer[config.Config]
	cfgPtr.Store(initialCfg)
	cfg := initialCfg

	// Capture server logs via pipe for the log collector.
	// Logs go to both the pipe (for event emission) and stderr (for console).
	logPipeR, logPipeW, err := os.Pipe()
	if err != nil {
		logger.Init("info")
		logger.FatalNoCtx().Err(err).Msg("failed to create log pipe")
	}
	logWriter := io.MultiWriter(logPipeW, os.Stderr)
	logger.InitWithWriter(logWriter, cfg.Server.LogLevel)

	logger.InfoNoCtx().Str("version", version.String()).Msg("starting gocache server")
	if cfgFile := v.ConfigFileUsed(); cfgFile != "" {
		logger.InfoNoCtx().Str("file", cfgFile).Msg("config loaded")
	}
	logger.InfoNoCtx().Str("addr", cfg.Server.GetAddr()).Msg("listening on")

	// Initialize core components (no operations yet — infrastructure setup).
	cacheInstance := cache.NewWithConfig(
		cfg.Memory.MaxMemoryMB,
		cache.ParseEvictionPolicy(cfg.Memory.EvictionPolicy),
	)
	engineInstance := engine.New(cacheInstance)
	blockingRegistry := blocking.NewRegistry()
	watchManager := watch.NewManager()
	cacheInstance.OnMutate = watchManager.NotifyMutation
	cacheInstance.OnMutateAll = watchManager.NotifyAll

	// Initialize infrastructure: tracker + event bus + log collector.
	tracker := serverOps.NewTracker()
	eventBus := serverEvents.NewBus()
	logCollector := logcollector.New(eventBus)
	logCollector.AddSource("server", logPipeR)

	// Initialize the server (before plugins so we have CoreCommandNames).
	srv := server.New(cfg.Server.GetAddr(), cacheInstance, engineInstance, cfg.Persistence.SnapshotFile, cfg.Server.RequirePass, blockingRegistry, watchManager)
	srv.SetEmitter(eventBus)
	srv.SetTracker(tracker)

	// Set up signal handling.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Plugin loading (NOT an operation — plugins must be ready before operations can be hooked) ---
	var pluginManager *pluginmgr.Manager
	var opHookExec *ophooks.Executor
	if cfg.Plugins.Enabled {
		pluginManager = pluginmgr.NewManager(cfg.Plugins, srv.CoreCommandNames(), srv)
		pluginManager.SetLogCollector(logCollector)
		if err := pluginManager.Start(ctx); err != nil {
			logger.FatalNoCtx().Err(err).Msg("failed to start plugin manager")
		}
		pluginManager.SetEventBus(eventBus)
		srv.SetPluginRouter(pluginManager.Router())
		srv.SetHookExecutor(cmdhooks.NewExecutor(pluginManager.HookRegistry(), cfg.Plugins.ShutdownTimeout))
		opHookExec = ophooks.NewExecutor(pluginManager.OpHookRegistry(), cfg.Plugins.ShutdownTimeout)
		srv.SetOpHookExecutor(opHookExec)
	}

	// --- ServerBootstrap operation (after plugins, so operation hooks can enrich) ---
	bootOp := tracker.Start(ops.TypeStartup, "")
	bootOp.Enrich("_version", version.String())
	bootOp.Enrich("_addr", cfg.Server.GetAddr())
	if opHookExec != nil && opHookExec.HasAny() {
		opHookExec.RunStartHooks(ctx, bootOp)
	}
	eventBus.Emit(events.NewOperationStart(bootOp.ID, string(bootOp.Type), "", bootOp.ContextSnapshot(false)))

	// LoadSnapshot operation.
	if cfg.Persistence.LoadOnStartup {
		snapOp := tracker.Start(ops.TypeSnapshot, bootOp.ID)
		snapOp.Enrich("_file", cfg.Persistence.SnapshotFile)
		snapOp.Enrich("_trigger", "startup")
		snapCtx := ops.WithContext(ctx, snapOp)
		if opHookExec != nil && opHookExec.HasAny() {
			opHookExec.RunStartHooks(snapCtx, snapOp)
		}
		eventBus.Emit(events.NewOperationStart(snapOp.ID, string(snapOp.Type), bootOp.ID, snapOp.ContextSnapshot(false)))
		if err := persistence.LoadSnapshot(snapCtx, cfg.Persistence.SnapshotFile, cacheInstance); err != nil {
			logger.Warn(snapCtx).Err(err).Msg("failed to load snapshot")
			snapOp.Fail(err.Error())
			if opHookExec != nil {
				opHookExec.RunCompleteHooks(snapOp)
			}
			eventBus.Emit(events.NewOperationComplete(snapOp.ID, string(snapOp.Type), uint64(snapOp.Duration().Nanoseconds()), "failed", err.Error(), snapOp.ContextSnapshot(false)))
			tracker.Fail(snapOp.ID, err.Error())
		} else {
			logger.Info(snapCtx).Str("file", cfg.Persistence.SnapshotFile).Msg("snapshot loaded")
			snapOp.Complete()
			if opHookExec != nil {
				opHookExec.RunCompleteHooks(snapOp)
			}
			eventBus.Emit(events.NewOperationComplete(snapOp.ID, string(snapOp.Type), uint64(snapOp.Duration().Nanoseconds()), "completed", "", snapOp.ContextSnapshot(false)))
			tracker.Complete(snapOp.ID)
		}
	}

	// Start engine.
	go engineInstance.Run()

	// Initialize and start workers.
	snapshotWorker := workers.NewSnapshotWorker(
		cacheInstance, engineInstance,
		cfg.Persistence.SnapshotInterval,
		cfg.Persistence.SnapshotFile,
	)
	cleanupWorker := workers.NewCleanupWorker(cacheInstance, engineInstance, cfg.Workers.CleanupInterval)
	snapshotWorker.SetTracker(tracker)
	snapshotWorker.SetEmitter(eventBus)
	if opHookExec != nil {
		snapshotWorker.SetOpHookExecutor(opHookExec)
		cleanupWorker.SetOpHookExecutor(opHookExec)
	}
	cleanupWorker.SetTracker(tracker)
	cleanupWorker.SetEmitter(eventBus)
	snapshotWorker.Start(ctx)
	cleanupWorker.Start(ctx)

	// Hot reload: config changes create config_reload operations.
	v.WatchConfig()
	v.OnConfigChange(func(e fsnotify.Event) {
		reloadOp := tracker.Start(ops.TypeConfigReload, "")
		reloadOp.Enrich("_file", e.Name)
		reloadCtx := ops.WithContext(context.Background(), reloadOp)
		if opHookExec != nil && opHookExec.HasAny() {
			opHookExec.RunStartHooks(reloadCtx, reloadOp)
		}

		newCfg, err := config.Reload(v)
		if err != nil {
			logger.Warn(reloadCtx).Err(err).Msg("failed to parse updated config")
			reloadOp.Fail(err.Error())
			if opHookExec != nil {
				opHookExec.RunCompleteHooks(reloadOp)
			}
			eventBus.Emit(events.NewOperationComplete(reloadOp.ID, string(reloadOp.Type), uint64(reloadOp.Duration().Nanoseconds()), "failed", err.Error(), reloadOp.ContextSnapshot(false)))
			tracker.Fail(reloadOp.ID, err.Error())
			return
		}
		logger.Info(reloadCtx).Str("file", e.Name).Msg("config reloaded")

		prev := cfgPtr.Load()
		if newCfg.Server.GetAddr() != prev.Server.GetAddr() {
			logger.Warn(reloadCtx).Msg("server address/port changes require a restart")
		}

		snapshotWorker.UpdateInterval(newCfg.Persistence.SnapshotInterval)
		snapshotWorker.UpdateFile(newCfg.Persistence.SnapshotFile)
		cleanupWorker.UpdateInterval(newCfg.Workers.CleanupInterval)
		cacheInstance.SetMemoryLimit(
			reloadCtx,
			newCfg.Memory.MaxMemoryMB,
			cache.ParseEvictionPolicy(newCfg.Memory.EvictionPolicy),
		)

		cfgPtr.Store(newCfg)
		reloadOp.Complete()
		if opHookExec != nil {
			opHookExec.RunCompleteHooks(reloadOp)
		}
		eventBus.Emit(events.NewOperationComplete(reloadOp.ID, string(reloadOp.Type), uint64(reloadOp.Duration().Nanoseconds()), "completed", "", reloadOp.ContextSnapshot(false)))
		tracker.Complete(reloadOp.ID)
	})

	// ServerBootstrap complete.
	bootOp.Complete()
	if opHookExec != nil {
		opHookExec.RunCompleteHooks(bootOp)
	}
	eventBus.Emit(events.NewOperationComplete(bootOp.ID, string(bootOp.Type), uint64(bootOp.Duration().Nanoseconds()), "completed", "", bootOp.ContextSnapshot(false)))
	tracker.Complete(bootOp.ID)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	// Start server in a goroutine.
	serverErrChan := make(chan error, 1)
	go func() {
		logger.InfoNoCtx().Msg("server ready to accept connections")
		if err := srv.Start(ctx); err != nil && err != context.Canceled {
			serverErrChan <- err
		}
	}()

	// Wait for shutdown signal or server error
	select {
	case sig := <-sigChan:
		logger.InfoNoCtx().Str("signal", sig.String()).Msg("received signal")
		handleShutdown(srv, snapshotWorker, cleanupWorker, engineInstance, cacheInstance, cfgPtr.Load(), blockingRegistry, pluginManager, tracker, eventBus, opHookExec, sig.String())
	case err := <-serverErrChan:
		logger.ErrorNoCtx().Err(err).Msg("server error")
		handleShutdown(srv, snapshotWorker, cleanupWorker, engineInstance, cacheInstance, cfgPtr.Load(), blockingRegistry, pluginManager, tracker, eventBus, opHookExec, "error: "+err.Error())
		os.Exit(1)
	}

	// Close the log pipe so the collector reader gets EOF.
	logPipeW.Close()
	logCollector.Wait()
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
	tracker *serverOps.Tracker,
	eventBus *serverEvents.Bus,
	opHookExec *ophooks.Executor,
	reason string,
) {
	// Create shutdown operation — plugins see this via operation hooks
	// before they are shut down.
	var shutdownOp *ops.Operation
	shutdownCtx := context.Background()
	if tracker != nil {
		shutdownOp = tracker.Start(ops.TypeShutdown, "")
		shutdownOp.Enrich("_reason", reason)
		shutdownCtx = ops.WithContext(shutdownCtx, shutdownOp)
		if opHookExec != nil && opHookExec.HasAny() {
			opHookExec.RunStartHooks(shutdownCtx, shutdownOp)
		}
		eventBus.Emit(events.NewServerShutdown(reason).WithOperationID(shutdownOp.ID))
	}

	logger.Info(shutdownCtx).Msg("starting graceful shutdown sequence")

	// Unblock all waiting BLPOP/BRPOP clients first so their connections can close.
	blockingRegistry.Shutdown()

	shutdownTimeout := 10 * time.Second
	logger.Info(shutdownCtx).Str("step", "1/6").Dur("timeout", shutdownTimeout).Msg("shutting down server")
	if err := srv.Shutdown(shutdownTimeout); err != nil {
		logger.Warn(shutdownCtx).Err(err).Msg("server shutdown error")
	}

	// Fire operation complete hooks BEFORE shutting down plugins
	// so gobservability can finalize the shutdown span.
	if shutdownOp != nil && opHookExec != nil {
		opHookExec.RunCompleteHooks(shutdownOp)
	}

	// Shutdown plugins.
	if pluginManager != nil {
		logger.Info(shutdownCtx).Str("step", "2/6").Msg("shutting down plugins")
		pluginManager.Shutdown(cfg.Plugins.ShutdownTimeout)
	}

	logger.Info(shutdownCtx).Str("step", "3/6").Msg("stopping background workers")
	snapshotWorker.Stop()
	cleanupWorker.Stop()

	logger.Info(shutdownCtx).Str("step", "4/6").Str("file", cfg.Persistence.SnapshotFile).Msg("saving final snapshot")
	if err := persistence.SaveSnapshot(shutdownCtx, cfg.Persistence.SnapshotFile, cacheInstance); err != nil {
		logger.Warn(shutdownCtx).Err(err).Msg("failed to save final snapshot")
	} else {
		logger.Info(shutdownCtx).Msg("final snapshot saved successfully")
	}

	logger.Info(shutdownCtx).Str("step", "5/6").Msg("stopping engine")
	engineInstance.Stop()

	if shutdownOp != nil {
		shutdownOp.Complete()
		if eventBus != nil {
			eventBus.Emit(events.NewOperationComplete(shutdownOp.ID, string(shutdownOp.Type), uint64(shutdownOp.Duration().Nanoseconds()), "completed", "", shutdownOp.ContextSnapshot(false)))
		}
		tracker.Complete(shutdownOp.ID)
	}

	logger.Info(shutdownCtx).Str("step", "6/6").Msg("shutdown complete")
}
