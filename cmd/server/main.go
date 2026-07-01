package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gocache/api/command"
	apictx "gocache/api/context"
	"gocache/api/events"
	apiobs "gocache/api/observability"
	ops "gocache/api/operations"
	apipersistence "gocache/api/persistence"
	"gocache/api/version"
	"gocache/commons/crashdump"
	"gocache/commons/logger"
	commonobs "gocache/commons/observability"
	"gocache/pkg/benchstats"
	"gocache/pkg/blocking"
	"gocache/pkg/bootstate"
	"gocache/pkg/cache"
	"gocache/pkg/config"
	"gocache/pkg/engine"
	serverEvents "gocache/pkg/events"
	"gocache/pkg/logcollector"
	commandmetrics "gocache/pkg/metrics"
	"gocache/pkg/persistence"
	"gocache/pkg/plugin/cmdhooks"
	pluginmgr "gocache/pkg/plugin/manager"
	"gocache/pkg/plugin/ophooks"
	"gocache/pkg/server"
	"gocache/pkg/watch"
	"gocache/pkg/workers"
	"gocache/sdk/embedded"

	// Embedded persistence plugin — registers itself via init() per
	// ADR-0008 (config contract) and ADR-0006 (physical location under
	// plugins/). Selecting a different snapshot backend is a swap of
	// this import line. The plugin receives a typed apiconfig.PluginConfig
	// view and never imports pkg/* — the layering rule stays mechanical.
	_ "gocache/plugins/snapshot"

	// Embedded plugins — compile-time-linked observability hooks that run
	// before config.Load and survive panics. See pkg/embedded for details.
	// Each blank import resolves regardless of build tags (every plugin
	// package carries a tagless doc.go); the tag-gated file inside the
	// package is what actually registers init(). Pick which ones by
	// setting the PLUGINS build arg on the Docker image, or by passing
	// -tags=crashdump,lifecycleotlp to `go build` directly.
	_ "gocache/plugins/aof"
	_ "gocache/plugins/crashdump"
	_ "gocache/plugins/lifecycleotlp"

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

	// Steady-state OperationTracker defaults are intentionally hardcoded in this
	// production wiring slice. The tracker starts with three segments per shard and
	// enables the bounded hot-shard growth monitor without adding deployment knobs.
	steadyStateOperationTrackerShardCount          = 8
	steadyStateOperationTrackerMinSegmentsPerShard = 3
	steadyStateOperationTrackerMaxSegmentsPerShard = 4
	steadyStateOperationTrackerSegmentSize         = 256
	// Match the initially preallocated slots so accepted operations are retained
	// for the projection/drain worker instead of being dropped/recycled.
	steadyStateOperationTrackerCompletedRingPerShard = steadyStateOperationTrackerMinSegmentsPerShard * steadyStateOperationTrackerSegmentSize
	steadyStateOperationTrackerDrainInterval         = 10 * time.Millisecond
	// Reserve high internal-id ranges for steady-state server lifecycle scopes
	// created outside pkg/pipeline's per-command sequence. Keep these distinct
	// from pkg/server, pkg/workers, and pkg/plugin ranges that share the manager.
	steadyStateStartupOperationIdentityBase      apiobs.InternalOperationIdentity = 1 << 54
	steadyStateConfigReloadOperationIdentityBase apiobs.InternalOperationIdentity = 1 << 53
	steadyStateShutdownOperationIdentityBase     apiobs.InternalOperationIdentity = 1 << 52

	// Env overrides for the crash-survivability layer. Keeping them here
	// (not in pkg/config) so they apply from line 1 of main(), before any
	// YAML has been parsed.
	envCrashdumpDir = "GOCACHE_CRASHDUMP_DIR"
	envBootState    = "GOCACHE_BOOT_STATE_FILE"

	defaultCrashdumpDir = crashdump.DefaultCrashdumpDir
	defaultBootState    = "boot.state"

	// Named boot stages written to the boot.state marker. A previous-run
	// file that doesn't show StageRunning at startup means the prior
	// process crashed at that stage.
	stageEmbeddedBoot  = "embedded_boot"
	stageConfigLoad    = "config_load"
	stageCoreInit      = "core_init"
	stagePluginLoad    = "plugin_load"
	stageSnapshotLoad  = "snapshot_load"
	stageWorkersStart  = "workers_start"
	stageListenerStart = "listener_start"
)

var (
	steadyStateStartupOperationSequence      atomic.Uint64
	steadyStateConfigReloadOperationSequence atomic.Uint64
	steadyStateShutdownOperationSequence     atomic.Uint64
)

func newSteadyStateOperationTrackerManager() *commonobs.SlotOperationTrackerManager {
	return commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            steadyStateOperationTrackerShardCount,
		MinSegmentsPerShard:   steadyStateOperationTrackerMinSegmentsPerShard,
		MaxSegmentsPerShard:   steadyStateOperationTrackerMaxSegmentsPerShard,
		SegmentSize:           steadyStateOperationTrackerSegmentSize,
		MagazineCapacity:      16,
		CompletedRingPerShard: steadyStateOperationTrackerCompletedRingPerShard,
		MaxChunksPerClass:     int64(steadyStateOperationTrackerMaxSegmentsPerShard * steadyStateOperationTrackerSegmentSize),
		HotShardGrowth: commonobs.HotShardGrowthConfig{
			Enabled:           true,
			MaxGrowthSegments: 4,
		},
	})
}

func startSteadyStateOperationTelemetryScope(manager *commonobs.SlotOperationTrackerManager, sequence *atomic.Uint64, identityBase apiobs.InternalOperationIdentity, op *ops.Operation) commonobs.OperationScope {
	if manager == nil || sequence == nil || op == nil {
		return commonobs.OperationScope{}
	}
	next := sequence.Add(1)
	if next == 0 {
		next = sequence.Add(1)
	}
	operation := identityBase + apiobs.InternalOperationIdentity(next)
	ref := apiobs.NewOperationRef(op.ID, op.ParentID)
	handle, ok := manager.StartOperationWithMetadata(operation, apiobs.NewParentRef(op.ParentID), 0, commonobs.OperationSnapshotMetadata{
		Type:          string(op.Type),
		Ref:           ref,
		StartUnixNano: op.StartTime.UnixNano(),
	}, nil)
	if !ok {
		return commonobs.OperationScope{}
	}
	scope := commonobs.NewOperationScope(manager, handle, operation, ref)
	scope.ContextUpdateStrings(
		command.OperationID, op.ID,
		command.StartNs, strconv.FormatInt(op.StartTime.UnixNano(), 10),
		"_operation_type", string(op.Type),
		"_parent_operation_id", op.ParentID,
	)
	recordOperationContext(scope, op)
	scope.OperationStartString(string(op.Type),
		command.OperationID, op.ID,
		"_operation_type", string(op.Type),
		"_parent_operation_id", op.ParentID,
	)
	return scope
}

func recordOperationContext(scope commonobs.OperationScope, op *ops.Operation) {
	if scope.IsZero() || op == nil {
		return
	}
	for key, value := range op.ContextSnapshot(false) {
		scope.ContextUpdateStrings(key, value)
	}
}

func startStartupTelemetryScope(manager *commonobs.SlotOperationTrackerManager, startupOp *ops.Operation) commonobs.OperationScope {
	return startSteadyStateOperationTelemetryScope(manager, &steadyStateStartupOperationSequence, steadyStateStartupOperationIdentityBase, startupOp)
}

func startConfigReloadTelemetryScope(manager *commonobs.SlotOperationTrackerManager, reloadOp *ops.Operation) commonobs.OperationScope {
	return startSteadyStateOperationTelemetryScope(manager, &steadyStateConfigReloadOperationSequence, steadyStateConfigReloadOperationIdentityBase, reloadOp)
}

func startShutdownTelemetryScope(manager *commonobs.SlotOperationTrackerManager, shutdownOp *ops.Operation, reason string) commonobs.OperationScope {
	scope := startSteadyStateOperationTelemetryScope(manager, &steadyStateShutdownOperationSequence, steadyStateShutdownOperationIdentityBase, shutdownOp)
	if scope.IsZero() {
		return scope
	}
	scope.ContextUpdateStrings("_reason", reason)
	return scope
}

func emitShutdownSignalTelemetry(manager *commonobs.SlotOperationTrackerManager, drainWorker *server.OperationTrackerDrainWorker, reason, parentID string) {
	signalOp := ops.New(ops.TypeShutdown, parentID)
	signalOp.Enrich("_reason", reason)
	signalOp.Enrich("_scope", "shutdown_signal")
	signalScope := startShutdownTelemetryScope(manager, signalOp, reason)
	if signalScope.IsZero() {
		return
	}
	signalScope.ContextUpdateStrings("_scope", "shutdown_signal")
	signalScope.EventString(string(events.ServerShutdown), command.OperationID, signalOp.ID, "_reason", reason)
	signalOp.Complete()
	finishLifecycleTelemetryScope(signalScope, signalOp, commonobs.SlotTerminalFinished, "")
	if drainWorker != nil {
		drainWorker.DrainOnce()
	}
}

func recordConfigReloadLog(scope commonobs.OperationScope, level apiobs.TelemetryLogLevel, message string, err error) bool {
	if scope.IsZero() {
		return false
	}
	record := apiobs.NewLogRecordString(scope.Operation(), level, message)
	record.TimestampUnixNano = time.Now().UnixNano()
	if err != nil {
		record.AddFieldString("error", err.Error())
	}
	return scope.Record(record)
}

func recordShutdownLog(scope commonobs.OperationScope, level apiobs.TelemetryLogLevel, message string, fields ...string) bool {
	if scope.IsZero() {
		return false
	}
	record := apiobs.NewLogRecordString(scope.Operation(), level, message)
	record.TimestampUnixNano = time.Now().UnixNano()
	for i := 0; i+1 < len(fields); i += 2 {
		record.AddFieldString(fields[i], fields[i+1])
	}
	return scope.Record(record)
}

func finishLifecycleTelemetryScope(scope commonobs.OperationScope, op *ops.Operation, terminal commonobs.SlotTerminalStatus, reason string) bool {
	if scope.IsZero() || op == nil {
		return false
	}
	elapsedNs := uint64(op.Duration().Nanoseconds())
	status := "completed"
	if terminal == commonobs.SlotTerminalFailed {
		status = "failed"
	}
	if reason != "" {
		scope.ContextUpdateStrings(command.ElapsedNs, strconv.FormatUint(elapsedNs, 10), command.ErrorKey, reason)
	} else {
		scope.ContextUpdateStrings(command.ElapsedNs, strconv.FormatUint(elapsedNs, 10))
	}
	scope.OperationFinishString(string(op.Type), elapsedNs,
		command.OperationID, op.ID,
		"_operation_type", string(op.Type),
		"_status", status,
		command.ElapsedNs, strconv.FormatUint(elapsedNs, 10),
		command.ErrorKey, reason,
	)
	return scope.Finish(terminal)
}

func activeCrashdumpSnapshots(trackers ...*commonobs.SlotOperationTrackerManager) []crashdump.OpSnapshot {
	var snapshots []crashdump.OpSnapshot
	for _, tracker := range trackers {
		if tracker == nil {
			continue
		}
		for _, active := range tracker.ActiveOperationSnapshots() {
			id := active.Ref.ID.String()
			if id == "" {
				id = strconv.FormatInt(int64(active.Operation), 10)
			}
			started := time.Time{}
			if active.StartUnixNano != 0 {
				started = time.Unix(0, active.StartUnixNano)
			}
			snapshots = append(snapshots, crashdump.OpSnapshot{
				ID:       id,
				Type:     active.Type,
				ParentID: active.Ref.ParentID.String(),
				Started:  started,
				Context:  apictx.RedactSecrets(active.Context),
			})
		}
	}
	return snapshots
}

func main() {
	// Define CLI flags — all optional; they override config file and env vars
	pflag.String("config", defaultConfigFile, "path to config file (.yaml or .json)")
	pflag.String("address", "", "server listen address (overrides config)")
	pflag.Int("port", 0, "server listen port (overrides config)")
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

	// Telemetry managers are assigned as boot progresses. The top-level
	// crashdump defer snapshots whichever managers are live at panic time.
	var startupTracker *commonobs.SlotOperationTrackerManager
	var steadyStateOperationTracker *commonobs.SlotOperationTrackerManager

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
				Dir:             crashDir,
				Version:         version.String(),
				BootStage:       stage,
				ActiveSnapshots: activeCrashdumpSnapshots(startupTracker, steadyStateOperationTracker),
			})
			panic(r) // re-raise so runtime stacktrace + non-zero exit still happen
		}
	}()

	// Process-wide context — used for both embedded plugin lifecycle and the
	// rest of boot. Declared early so embedded.BootAll runs under it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Embedded plugins run BEFORE config.Load so they can observe boot-time
	// failures (e.g. a config parse error surfaces as an lifecycle OTLP span). Defer
	// ShutdownAll immediately so it fires on normal exit AND during a panic
	// unwind — giving exporters a final flush even if main() crashes.
	_ = bootstate.Write(bootStateFile, stageEmbeddedBoot)
	embBootOp := ops.New(ops.TypeStartup, "")
	embBootOp.Enrich("_boot_stage", "embedded_boot")
	embedded.BootAll(ops.WithContext(ctx, embBootOp))
	embBootOp.Complete()
	defer func() {
		embShutOp := ops.New(ops.TypeShutdown, "")
		embShutOp.Enrich("_scope", "embedded_plugins")
		embedded.ShutdownAll(ops.WithContext(ctx, embShutOp))
		embShutOp.Complete()
	}()

	// Load configuration: CLI flags > env vars (GOCACHE_*) > config file > defaults
	_ = bootstate.Write(bootStateFile, stageConfigLoad)
	initialCfg, err := config.Load(pflag.CommandLine)
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
	defer logPipeR.Close()
	logWriter := io.MultiWriter(logPipeW, os.Stderr)
	logger.InitWithWriter(logWriter, cfg.Server.LogLevel)

	startupTracker = commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		MagazineCapacity:      16,
		CompletedRingPerShard: 1,
	})
	startupOperation := apiobs.InternalOperationIdentity(1)
	startupIdentity, err := (commonobs.UUIDv7Strategy{}).Render(apiobs.OperationIdentityInput{
		Internal:      startupOperation,
		StartUnixNano: time.Now().UnixNano(),
		Sequence:      uint64(startupOperation),
	})
	if err != nil {
		logger.FatalNoCtx().Err(err).Msg("failed to render startup operation identity")
	}
	startupRef := startupIdentity.Ref()
	startupHandle, ok := startupTracker.StartOperationWithMetadata(startupOperation, apiobs.ParentRef{}, 0, commonobs.OperationSnapshotMetadata{
		Type:          string(ops.TypeStartup),
		Ref:           startupRef,
		StartUnixNano: time.Now().UnixNano(),
	}, nil)
	if !ok {
		logger.FatalNoCtx().Msg("failed to allocate startup telemetry slot")
	}
	startupScope := commonobs.NewOperationScope(startupTracker, startupHandle, startupOperation, startupRef)
	startupLogs := commonobs.StartupLogMaterializer{}

	// Hand the parsed config to embedded plugins so they can upgrade
	// env-var-only defaults with YAML-backed values (e.g. lifecycle OTLP endpoint).
	cfgOp := ops.New(ops.TypeConfigReload, "")
	cfgOp.Enrich("_boot_stage", "config_loaded")
	embedded.ConfigLoadedAll(ops.WithContext(ctx, cfgOp), cfg)
	cfgOp.Complete()

	startupRecord := apiobs.NewLogRecordString(startupScope.Operation(), apiobs.TelemetryLogLevelInfo, "starting gocache server")
	startupRecord.AddFieldString("version", version.String())
	startupLogs.LogRecord(startupScope, startupRecord)
	if n := embedded.Count(); n > 0 {
		startupRecord = apiobs.NewLogRecordString(startupScope.Operation(), apiobs.TelemetryLogLevelInfo, "embedded plugins loaded")
		startupRecord.AddFieldString("count", strconv.Itoa(n))
		startupRecord.AddFieldString("names", strings.Join(embedded.Names(), ","))
		startupLogs.LogRecord(startupScope, startupRecord)
	}
	if cfgFile := config.ConfigFileUsed(); cfgFile != "" {
		startupRecord = apiobs.NewLogRecordString(startupScope.Operation(), apiobs.TelemetryLogLevelInfo, "config loaded")
		startupRecord.AddFieldString("file", cfgFile)
		startupLogs.LogRecord(startupScope, startupRecord)
	}
	startupRecord = apiobs.NewLogRecordString(startupScope.Operation(), apiobs.TelemetryLogLevelInfo, "configured listen address")
	startupRecord.AddFieldString("addr", cfg.Server.GetAddr())
	startupLogs.LogRecord(startupScope, startupRecord)

	// Initialize core components (no operations yet — infrastructure setup).
	_ = bootstate.Write(bootStateFile, stageCoreInit)
	cacheInstance := cache.NewWithShards(
		cfg.Memory.CacheShards,
		cfg.Memory.MaxMemoryMB,
		cache.ParseEvictionPolicy(cfg.Memory.EvictionPolicy),
	)
	cacheInstance.SetPackedThresholds(cache.PackedThresholds{
		HashMaxEntries: cfg.Memory.HashMaxPackedEntries,
		HashMaxValue:   cfg.Memory.HashMaxPackedValue,
		SetMaxEntries:  cfg.Memory.SetMaxPackedEntries,
		SetMaxValue:    cfg.Memory.SetMaxPackedValue,
		ZSetMaxEntries: cfg.Memory.ZSetMaxPackedEntries,
		ZSetMaxValue:   cfg.Memory.ZSetMaxPackedValue,
		ListMaxBytes:   cfg.Memory.ListMaxPackedSize,
	})
	engineInstance := engine.New(cacheInstance)
	blockingRegistry := blocking.NewRegistry()
	watchManager := watch.NewManager()
	cacheInstance.SetOnMutate(watchManager.NotifyMutation)
	cacheInstance.SetOnMutateAll(watchManager.NotifyAll)

	// tracker was created above main() for the crashdump defer; reuse it.
	eventBus := serverEvents.NewBusWithCapacity(cfg.Events.ReplayCapacity)
	logCollector := logcollector.New(eventBus)
	logCollector.AddSource("server", logPipeR)

	// Initialize the server (before plugins so we have CoreCommandNames).
	srv := server.New(cfg.Server.GetAddr(), cacheInstance, engineInstance, cfg.Server.RequirePass, blockingRegistry, watchManager)
	srv.SetEmitter(eventBus)
	// Startup telemetry has its own one-shot tracker; steady-state commands use
	// this bounded commons manager from the first accepted command onward.
	steadyStateOperationTracker = newSteadyStateOperationTrackerManager()
	srv.SetOperationTrackerManager(steadyStateOperationTracker)
	benchstats.SetOperationTrackerManager(steadyStateOperationTracker)
	operationDrainWorker := server.NewOperationTrackerDrainWorker(steadyStateOperationTracker, steadyStateOperationTrackerDrainInterval)
	operationDrainWorker.SetWorkerCount(8)
	operationDrainWorker.SetEmitter(eventBus)

	// --- Plugin loading (NOT an operation — plugins must be ready before operations can be hooked) ---
	_ = bootstate.Write(bootStateFile, stagePluginLoad)
	var pluginManager *pluginmgr.Manager
	var opHookExec *ophooks.Executor
	if cfg.Plugins.Enabled {
		commandMetrics := commandmetrics.NewCommandCollector()
		srv.SetCommandMetrics(commandMetrics)
		pluginManager = pluginmgr.NewManager(cfg.Plugins, srv.CoreCommandNames(), srv)
		pluginManager.SetLogCollector(logCollector)
		pluginManager.SetOperationTrackerManager(steadyStateOperationTracker)
		pluginManager.SetClientPusher(srv.ConnRegistry())
		pluginManager.SetEventBus(eventBus)
		pluginManager.SetCommandMetrics(commandMetrics)
		if err := pluginManager.Start(ctx); err != nil {
			finishStartupFatal(startupLogs, startupScope, err, "failed to start plugin manager")
		}
		srv.SetPluginRouter(pluginManager.Router())
		srv.SetHookExecutor(cmdhooks.NewExecutor(pluginManager.HookRegistry(), cfg.Plugins.ShutdownTimeout))
		opHookExec = ophooks.NewExecutor(pluginManager.OpHookRegistry(), cfg.Plugins.ShutdownTimeout)
		opHookExec.SetOperationTrackerManager(steadyStateOperationTracker)
		opHookExec.SetMinRestartInterval(cfg.Plugins.MinRestartIntervalForReplay)
		// Replay synthesizes PhaseStart for every active op that started
		// before an operation-hook plugin joined, so late subscribers can
		// reconstruct operation state from the original process timeline.
		pluginManager.OpHookRegistry().SetOnRegister(opHookExec.Replay)
		srv.SetOpHookExecutor(opHookExec)
	}

	// --- ServerBootstrap operation (after plugins, so operation hooks can enrich) ---
	bootOp := ops.New(ops.TypeStartup, "")
	bootOp.Enrich("_version", version.String())
	bootOp.Enrich("_addr", cfg.Server.GetAddr())
	bootScope := startStartupTelemetryScope(steadyStateOperationTracker, bootOp)
	if opHookExec != nil && opHookExec.HasAny() {
		opHookExec.RunStartHooks(ctx, bootOp)
		recordOperationContext(bootScope, bootOp)
	}

	// Build all registered persistence providers generically. Each
	// blank-imported plugin (plugins/snapshot, plugins/aof, …) called
	// RegisterProvider in its init(). The server iterates them without
	// knowing what any specific plugin does — it collects Sources,
	// Sinks, and Snapshotters, then wires them into the coordinator.
	var (
		primarySource apipersistence.Source
		sinks         []apipersistence.Sink
		backends      []*apipersistence.Backend
	)
	for _, prov := range apipersistence.RegisteredProviders() {
		pluginCfg := config.PluginConfigFor(prov.Name())
		backend, err := prov.Build(pluginCfg, cacheInstance)
		if err != nil {
			startupRecord = apiobs.NewLogRecordString(startupScope.Operation(), apiobs.TelemetryLogLevelError, "persistence plugin Build failed; skipping")
			startupRecord.AddFieldString("plugin", prov.Name())
			startupRecord.AddFieldString("error", err.Error())
			startupLogs.LogRecord(startupScope, startupRecord)
			continue
		}
		if backend.Source != nil && primarySource == nil {
			primarySource = backend.Source
		}
		if backend.Sink != nil {
			sinks = append(sinks, backend.Sink)
		}
		if backend.OnReload != nil {
			config.OnPluginReload(prov.Name(), backend.OnReload.OnConfigReload)
		}
		backends = append(backends, backend)
		startupRecord = apiobs.NewLogRecordString(startupScope.Operation(), apiobs.TelemetryLogLevelInfo, "persistence backend loaded")
		startupRecord.AddFieldString("plugin", prov.Name())
		startupLogs.LogRecord(startupScope, startupRecord)
	}

	coordinator := persistence.New(primarySource, sinks...)
	coordinator.SetStore(cacheInstance)
	for _, b := range backends {
		if b.Snapshotter != nil {
			coordinator.RegisterSnapshotter(b.Snapshotter)
		}
	}
	srv.SetPersistenceFeed(coordinator)

	// Bind deferred plugin commands now that the coordinator is ready.
	for _, b := range backends {
		if b.Commands == nil {
			continue
		}
		for _, cmd := range b.Commands(coordinator) {
			srv.RegisterEmbeddedCommand(cmd.Name, cmd.Fn, cmd.Spec)
		}
	}

	// LoadSnapshot operation.
	_ = bootstate.Write(bootStateFile, stageSnapshotLoad)
	if cfg.Persistence.LoadOnStartup {
		snapOp := ops.New(ops.TypeSnapshot, bootOp.ID)
		snapOp.Enrich(command.TriggerKey, "startup")
		snapScope := startStartupTelemetryScope(steadyStateOperationTracker, snapOp)
		snapCtx := ops.WithContext(ctx, snapOp)
		if opHookExec != nil && opHookExec.HasAny() {
			opHookExec.RunStartHooks(snapCtx, snapOp)
			recordOperationContext(snapScope, snapOp)
		}
		if _, err := coordinator.BootInto(snapCtx); err != nil {
			startupRecord = apiobs.NewLogRecordString(startupScope.Operation(), apiobs.TelemetryLogLevelWarn, "failed to load snapshot")
			startupRecord.AddFieldString("error", err.Error())
			startupLogs.LogRecord(startupScope, startupRecord)
			snapOp.Fail(err.Error())
			if opHookExec != nil {
				opHookExec.RunCompleteHooks(snapOp)
			}
			finishLifecycleTelemetryScope(snapScope, snapOp, commonobs.SlotTerminalFailed, err.Error())
		} else {
			startupLogs.LogString(startupScope, apiobs.TelemetryLogLevelInfo, "snapshot loaded")
			snapOp.Complete()
			if opHookExec != nil {
				opHookExec.RunCompleteHooks(snapOp)
			}
			finishLifecycleTelemetryScope(snapScope, snapOp, commonobs.SlotTerminalFinished, "")
		}
	}

	// Start the persistence coordinator AFTER boot so the LSN cursor
	// reflects the recovered snapshot. With no sinks registered (current
	// configuration), Start is a no-op aside from arming the lifecycle —
	// HasSinks remains false and the dispatcher's emission fast-path
	// short-circuits. Stop runs in the shutdown sequence below.
	coordinator.Start(ctx)

	// Initialize and start workers.
	_ = bootstate.Write(bootStateFile, stageWorkersStart)
	snapshotWorker := workers.NewSnapshotWorker(
		cacheInstance, engineInstance,
		cfg.Persistence.SnapshotInterval,
	)
	cleanupWorker := workers.NewCleanupWorker(cacheInstance, engineInstance, cfg.Workers.CleanupInterval)
	cleanupWorker.SetPersistenceFeed(coordinator)
	snapshotWorker.SetPersistenceAPI(coordinator)
	snapshotWorker.SetOperationTrackerManager(steadyStateOperationTracker)
	cleanupWorker.SetOperationTrackerManager(steadyStateOperationTracker)
	snapshotWorker.Start(ctx)
	cleanupWorker.Start(ctx)
	operationDrainWorker.Start(ctx)

	// Hot reload: server-orchestration knobs only. Plugins subscribe
	// to config.OnPluginReload independently (per ADR-0008) and handle
	// their own re-config there. The fsnotify watcher + fan-out
	// multiplexer are installed inside config.Load.
	config.OnReload(func() {
		reloadOp := ops.New(ops.TypeConfigReload, "")
		reloadScope := startConfigReloadTelemetryScope(steadyStateOperationTracker, reloadOp)
		reloadCtx := ops.WithContext(context.Background(), reloadOp)
		if opHookExec != nil && opHookExec.HasAny() {
			opHookExec.RunStartHooks(reloadCtx, reloadOp)
			recordOperationContext(reloadScope, reloadOp)
		}

		newCfg, err := config.Reload()
		if err != nil {
			recordConfigReloadLog(reloadScope, apiobs.TelemetryLogLevelWarn, "failed to parse updated config", err)
			reloadOp.Fail(err.Error())
			if opHookExec != nil {
				opHookExec.RunCompleteHooks(reloadOp)
			}
			finishLifecycleTelemetryScope(reloadScope, reloadOp, commonobs.SlotTerminalFailed, err.Error())
			return
		}
		recordConfigReloadLog(reloadScope, apiobs.TelemetryLogLevelInfo, "config reloaded", nil)

		prev := cfgPtr.Load()
		if newCfg.Server.GetAddr() != prev.Server.GetAddr() {
			recordConfigReloadLog(reloadScope, apiobs.TelemetryLogLevelWarn, "server address/port changes require a restart", nil)
		}

		snapshotWorker.UpdateInterval(newCfg.Persistence.SnapshotInterval)
		cleanupWorker.UpdateInterval(newCfg.Workers.CleanupInterval)
		cacheInstance.SetMemoryLimit(reloadScope, newCfg.Memory.MaxMemoryMB, cache.ParseEvictionPolicy(newCfg.Memory.EvictionPolicy))

		cfgPtr.Store(newCfg)
		reloadOp.Complete()
		if opHookExec != nil {
			opHookExec.RunCompleteHooks(reloadOp)
		}
		finishLifecycleTelemetryScope(reloadScope, reloadOp, commonobs.SlotTerminalFinished, "")
	})

	// ServerBootstrap complete.
	bootOp.Complete()
	if opHookExec != nil {
		opHookExec.RunCompleteHooks(bootOp)
	}
	finishLifecycleTelemetryScope(bootScope, bootOp, commonobs.SlotTerminalFinished, "")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	// Start server in a goroutine.
	_ = bootstate.Write(bootStateFile, stageListenerStart)
	serverErrChan := make(chan error, 1)
	startupTelemetry := server.StartupTelemetry{
		Scope: startupScope,
		Logs:  startupLogs,
		OnReady: func() {
			_ = bootstate.Write(bootStateFile, bootstate.StageRunning)
		},
	}
	go func() {
		if err := srv.Start(ctx, startupTelemetry); err != nil && !errors.Is(err, context.Canceled) {
			serverErrChan <- err
		}
	}()

	// Wait for shutdown signal or server error
	select {
	case sig := <-sigChan:
		logger.InfoNoCtx().Str("signal", sig.String()).Msg("received signal")
		handleShutdown(srv, snapshotWorker, cleanupWorker, operationDrainWorker, engineInstance, cfgPtr.Load(), blockingRegistry, pluginManager, steadyStateOperationTracker, opHookExec, coordinator, sig.String())
	case err := <-serverErrChan:
		handleShutdown(srv, snapshotWorker, cleanupWorker, operationDrainWorker, engineInstance, cfgPtr.Load(), blockingRegistry, pluginManager, steadyStateOperationTracker, opHookExec, coordinator, "error: "+err.Error())
		os.Exit(1)
	}

	// Close the log pipe so the collector reader gets EOF.
	logPipeW.Close()
	logCollector.Wait()
}

func finishStartupFatal(logs commonobs.StartupLogMaterializer, scope commonobs.OperationScope, err error, message string) {
	record := apiobs.NewLogRecordString(scope.Operation(), apiobs.TelemetryLogLevelFatal, message)
	if err != nil {
		record.AddFieldString("error", err.Error())
	}
	logs.LogRecord(scope, record)
	scope.Finish(commonobs.SlotTerminalFailed)
	os.Exit(1)
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
	operationDrainWorker *server.OperationTrackerDrainWorker,
	engineInstance *engine.Engine,
	cfg *config.Config,
	blockingRegistry *blocking.Registry,
	pluginManager *pluginmgr.Manager,
	steadyStateOperationTracker *commonobs.SlotOperationTrackerManager,
	opHookExec *ophooks.Executor,
	coordinator *persistence.Coordinator,
	reason string,
) {
	// Create shutdown operation — plugins see this via operation hooks before they
	// are shut down, while runtime telemetry is recorded through telemetry records.
	shutdownOp := ops.New(ops.TypeShutdown, "")
	shutdownOp.Enrich("_reason", reason)
	shutdownScope := startShutdownTelemetryScope(steadyStateOperationTracker, shutdownOp, reason)
	shutdownCtx := ops.WithContext(context.Background(), shutdownOp)
	if opHookExec != nil && opHookExec.HasAny() {
		opHookExec.RunStartHooks(shutdownCtx, shutdownOp)
		recordOperationContext(shutdownScope, shutdownOp)
	}

	recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelInfo, "starting graceful shutdown sequence")

	// Unblock all waiting BLPOP/BRPOP clients first so their connections can close.
	blockingRegistry.Shutdown()

	recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelInfo, "shutting down server", "step", "1/6", "timeout", serverShutdownTimeout.String())
	if err := srv.Shutdown(serverShutdownTimeout); err != nil {
		recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelWarn, "server shutdown error", "error", err.Error())
	}

	// Fire operation complete hooks before shutting down plugins so
	// subscribers can observe the shutdown marker.
	if opHookExec != nil {
		opHookExec.RunCompleteHooks(shutdownOp)
	}
	emitShutdownSignalTelemetry(steadyStateOperationTracker, operationDrainWorker, reason, shutdownOp.ID)

	// Shutdown plugins.
	if pluginManager != nil {
		recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelInfo, "shutting down plugins", "step", "2/6")
		pluginManager.Shutdown(cfg.Plugins.ShutdownTimeout)
	}

	recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelInfo, "stopping background workers", "step", "3/6")
	snapshotWorker.Stop()
	cleanupWorker.Stop()

	// Drain the persistence coordinator BEFORE the final snapshot so any
	// inflight mutations make it to their sinks. With no sinks registered
	// in this build, Stop is a no-op; with sinks, Stop blocks until each
	// sink's flush goroutine drains its buffer and Sink.Close returns.
	coordinator.Stop(shutdownCtx)

	recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelInfo, "saving final snapshot", "step", "4/6")
	if err := coordinator.Snapshot(shutdownCtx); err != nil {
		recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelWarn, "failed to save final snapshot", "error", err.Error())
	} else {
		recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelInfo, "final snapshot saved successfully")
	}

	recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelInfo, "stopping engine", "step", "5/6")
	engineInstance.Stop()

	shutdownOp.Complete()
	recordShutdownLog(shutdownScope, apiobs.TelemetryLogLevelInfo, "shutdown complete", "step", "6/6")
	finishLifecycleTelemetryScope(shutdownScope, shutdownOp, commonobs.SlotTerminalFinished, "")
	operationDrainWorker.Stop()
}
