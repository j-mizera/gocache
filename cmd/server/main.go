package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"gocache/api/command"
	"gocache/api/events"
	"gocache/api/logger"
	ops "gocache/api/operations"
	"gocache/pkg/blocking"
	"gocache/pkg/bootstate"
	"gocache/pkg/cache"
	"gocache/pkg/config"
	"gocache/pkg/crashdump"
	"gocache/pkg/embedded"
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

	// Embedded plugins — compile-time-linked observability hooks that run
	// before config.Load and survive panics. See pkg/embedded for details.
	// Each blank import resolves regardless of build tags (every plugin
	// package carries a tagless doc.go); the tag-gated file inside the
	// package is what actually registers init(). Pick which ones by
	// setting the PLUGINS build arg on the Docker image, or by passing
	// -tags=crashdump,otlp to `go build` directly.
	_ "gocache/plugins/crashdump"
	_ "gocache/plugins/otlp"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/pflag"
)

// Entry-point defaults.
const (
	// defaultConfigFile is the config path used when --config is not passed.
	defaultConfigFile = "gocache.yaml"
	// serverShutdownTimeout is the time budget for the TCP server's
	// graceful Shutdown step in handleShutdown. Distinct from the ctx-cancel
	// path in pkg/server which has its own shorter timeout.
	serverShutdownTimeout = 10 * time.Second

	// Env overrides for the crash-survivability layer. Keeping them here
	// (not in pkg/config) so they apply from line 1 of main(), before any
	// YAML has been parsed.
	envCrashdumpDir = "GOCACHE_CRASHDUMP_DIR"
	envBootState    = "GOCACHE_BOOT_STATE_FILE"

	defaultCrashdumpDir = "crashes"
	defaultBootState    = "boot.state"

	// Named boot stages written to the boot.state marker. A previous-run
	// file that doesn't show StageRunning at startup means the prior
	// process crashed at that stage.
	stageEmbeddedBoot   = "embedded_boot"
	stageConfigLoad     = "config_load"
	stageCoreInit       = "core_init"
	stagePluginLoad     = "plugin_load"
	stageSnapshotLoad   = "snapshot_load"
	stageWorkersStart   = "workers_start"
	stageListenerStart  = "listener_start"
)

func main() {
	// Define CLI flags — all optional; they override config file and env vars
	pflag.String("config", defaultConfigFile, "path to config file (.yaml or .json)")
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

	// Resolve crash-survivability paths from env — they must work even if
	// config.Load later fails.
	crashDir := envOr(envCrashdumpDir, defaultCrashdumpDir)
	bootStateFile := envOr(envBootState, defaultBootState)

	// Infrastructure hoisted above embedded.BootAll: the tracker is needed
	// by the top-level crashdump recover (it snapshots Active() into the
	// dump). Creating it here is cheap — it's a map + mutex.
	tracker := serverOps.NewTracker()
	// processStart anchors replay_offset_ns for late-subscribing op-hook
	// plugins so reconstructed span timings match actual wall-clock order
	// instead of subscribe time. Captured here so the first op (which
	// tracker.Start creates moments later) gets a non-negative offset.
	processStart := time.Now()

	// Top-level crashdump defer — LAST line of defense. Registered first
	// so it survives for the entire main() call, including BootAll.
	// Writes a JSON dump to disk on any panic, then re-raises so the
	// runtime prints the stack trace and exits non-zero. The dump file
	// is picked up by the crashdump embedded plugin on the next boot.
	defer func() {
		if r := recover(); r != nil {
			stage := "unknown"
			if s, err := bootstate.Read(bootStateFile); err == nil {
				stage = s.Stage
			}
			_, _ = crashdump.WriteFromPanic(r, crashdump.Options{
				Dir:       crashDir,
				Version:   version.String(),
				BootStage: stage,
				ActiveOps: tracker.Active(),
			})
			panic(r) // re-raise so runtime stacktrace + non-zero exit still happen
		}
	}()

	// Process-wide context — used for both embedded plugin lifecycle and the
	// rest of boot. Declared early so embedded.BootAll runs under it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Embedded plugins run BEFORE config.Load so they can observe boot-time
	// failures (e.g. a config parse error surfaces as an OTLP span). Defer
	// ShutdownAll immediately so it fires on normal exit AND during a panic
	// unwind — giving exporters a final flush even if main() crashes.
	_ = bootstate.Write(bootStateFile, stageEmbeddedBoot)
	embedded.BootAll(ctx)
	defer embedded.ShutdownAll(ctx)

	// Load configuration: CLI flags > env vars (GOCACHE_*) > config file > defaults
	_ = bootstate.Write(bootStateFile, stageConfigLoad)
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

	// Hand the parsed config to embedded plugins so they can upgrade
	// env-var-only defaults with YAML-backed values (e.g. OTLP endpoint).
	embedded.ConfigLoadedAll(ctx, cfg)

	logger.InfoNoCtx().Str("version", version.String()).Msg("starting gocache server")
	if n := embedded.Count(); n > 0 {
		logger.InfoNoCtx().Int("count", n).Strs("names", embedded.Names()).Msg("embedded plugins loaded")
	}
	if cfgFile := v.ConfigFileUsed(); cfgFile != "" {
		logger.InfoNoCtx().Str("file", cfgFile).Msg("config loaded")
	}
	logger.InfoNoCtx().Str("addr", cfg.Server.GetAddr()).Msg("listening on")

	// Initialize core components (no operations yet — infrastructure setup).
	_ = bootstate.Write(bootStateFile, stageCoreInit)
	cacheInstance := cache.NewWithConfig(
		cfg.Memory.MaxMemoryMB,
		cache.ParseEvictionPolicy(cfg.Memory.EvictionPolicy),
	)
	engineInstance := engine.New(cacheInstance)
	blockingRegistry := blocking.NewRegistry()
	watchManager := watch.NewManager()
	cacheInstance.OnMutate = watchManager.NotifyMutation
	cacheInstance.OnMutateAll = watchManager.NotifyAll

	// tracker was created above main() for the crashdump defer; reuse it.
	eventBus := serverEvents.NewBusWithCapacity(cfg.Events.ReplayCapacity)
	logCollector := logcollector.New(eventBus)
	logCollector.AddSource("server", logPipeR)

	// Initialize the server (before plugins so we have CoreCommandNames).
	srv := server.New(cfg.Server.GetAddr(), cacheInstance, engineInstance, cfg.Persistence.SnapshotFile, cfg.Server.RequirePass, blockingRegistry, watchManager)
	srv.SetEmitter(eventBus)
	srv.SetTracker(tracker)

	// --- Plugin loading (NOT an operation — plugins must be ready before operations can be hooked) ---
	_ = bootstate.Write(bootStateFile, stagePluginLoad)
	var pluginManager *pluginmgr.Manager
	var opHookExec *ophooks.Executor
	if cfg.Plugins.Enabled {
		pluginManager = pluginmgr.NewManager(cfg.Plugins, srv.CoreCommandNames(), srv)
		pluginManager.SetLogCollector(logCollector)
		pluginManager.SetTracker(tracker)
		if err := pluginManager.Start(ctx); err != nil {
			logger.FatalNoCtx().Err(err).Msg("failed to start plugin manager")
		}
		pluginManager.SetEventBus(eventBus)
		srv.SetPluginRouter(pluginManager.Router())
		srv.SetHookExecutor(cmdhooks.NewExecutor(pluginManager.HookRegistry(), cfg.Plugins.ShutdownTimeout))
		opHookExec = ophooks.NewExecutor(pluginManager.OpHookRegistry(), cfg.Plugins.ShutdownTimeout)
		opHookExec.SetTracker(tracker)
		opHookExec.SetProcessStartTime(processStart)
		// Replay synthesizes PhaseStart for every active op that started
		// before this plugin joined, so late IPC subscribers (gobservability
		// and kin) can reconstruct spans rooted at process start instead of
		// at their own connect time.
		pluginManager.OpHookRegistry().SetOnRegister(opHookExec.Replay)
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
	_ = bootstate.Write(bootStateFile, stageSnapshotLoad)
	if cfg.Persistence.LoadOnStartup {
		snapOp := tracker.Start(ops.TypeSnapshot, bootOp.ID)
		snapOp.Enrich(command.FileKey, cfg.Persistence.SnapshotFile)
		snapOp.Enrich(command.TriggerKey, "startup")
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
	_ = bootstate.Write(bootStateFile, stageWorkersStart)
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
		reloadOp.Enrich(command.FileKey, e.Name)
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
	_ = bootstate.Write(bootStateFile, stageListenerStart)
	serverErrChan := make(chan error, 1)
	go func() {
		logger.InfoNoCtx().Msg("server ready to accept connections")
		if err := srv.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			serverErrChan <- err
		}
	}()
	_ = bootstate.Write(bootStateFile, bootstate.StageRunning)

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

// envOr returns the value of the named env var, or fallback when unset/empty.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
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

	logger.Info(shutdownCtx).Str("step", "1/6").Dur("timeout", serverShutdownTimeout).Msg("shutting down server")
	if err := srv.Shutdown(serverShutdownTimeout); err != nil {
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
