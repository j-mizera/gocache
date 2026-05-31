package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	apicommand "gocache/api/command"
	apiconfig "gocache/api/config"
	opctx "gocache/api/context"
	"gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"
	ops "gocache/api/operations"
	apiplugin "gocache/api/plugin"
	"gocache/api/scope"
	"gocache/commons/logger"
	"gocache/commons/transport"
	pkgconfig "gocache/pkg/config"
	serverEvents "gocache/pkg/events"
	commandmetrics "gocache/pkg/metrics"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/plugin"
	"gocache/pkg/plugin/cmdhooks"
	"gocache/pkg/plugin/ophooks"
	"gocache/pkg/plugin/permissions"
	"gocache/pkg/plugin/router"
)

// LogCollector is the interface for adding log sources.
// Defined here to avoid importing pkg/logcollector from the manager.
type LogCollector interface {
	AddSource(name string, r io.Reader)
}

// ClientPusher pushes raw RESP data to a specific client connection.
type ClientPusher interface {
	Push(connectionID string, data []byte) error
}

type eventBridgeMode string

const (
	eventBridgeModeFull      eventBridgeMode = "full"
	eventBridgeModeBridgeOff eventBridgeMode = "bridge-off"

	benchEventBridgeModeEnv = "GOCACHE_BENCH_EVENT_BRIDGE_MODE"
)

func benchEventBridgeMode() eventBridgeMode {
	switch os.Getenv(benchEventBridgeModeEnv) {
	case "", string(eventBridgeModeFull):
		return eventBridgeModeFull
	case string(eventBridgeModeBridgeOff):
		return eventBridgeModeBridgeOff
	default:
		return eventBridgeModeFull
	}
}

// Manager handles plugin lifecycle: discovery, fork/exec, registration,
// health monitoring, restart, and graceful shutdown.
//
// The lifecycle context.Context is not stored on the Manager. Start derives
// one from its caller and threads it into every spawned goroutine via closure
// or parameter. Shutdown calls the stored cancel function to terminate all
// in-flight goroutines and subprocesses.
type Manager struct {
	cfg            plugin.PluginsConfig
	listener       *transport.Listener
	registry       *Registry
	router         *router.Router
	hookRegistry   *cmdhooks.Registry
	opHookRegistry *ophooks.Registry
	scopeRegistry  *permissions.Registry
	queryRegistry  *QueryRegistry
	eventBus       *serverEvents.Bus
	logCollector   LogCollector
	tracker        *serverOps.Tracker
	clientPusher   ClientPusher
	commandMetrics *commandmetrics.CommandCollector

	// cancel terminates the lifecycle context derived inside Start.
	// nil before Start; reset to nil by Shutdown.
	cancel context.CancelFunc

	pluginConns             sync.Map // map[pluginName]*router.PluginConn
	commandMetricsConsumers sync.Map // map[pluginName]struct{}
	wg                      sync.WaitGroup
}

// NewManager creates a plugin manager with the given configuration.
// coreCommands is the list of command names handled by the core evaluator;
// plugin commands that shadow these will be rejected during registration.
func NewManager(cfg plugin.PluginsConfig, coreCommands []string, stateProvider ServerStateProvider) *Manager {
	reg := NewRegistry()
	qr := NewQueryRegistry()
	mgr := &Manager{
		cfg:            cfg,
		registry:       reg,
		router:         router.NewRouter(coreCommands),
		hookRegistry:   cmdhooks.NewRegistry(),
		opHookRegistry: ophooks.NewRegistry(),
		scopeRegistry:  permissions.NewRegistry(),
		queryRegistry:  qr,
	}
	RegisterBuiltinHandlers(qr, reg, mgr.pluginIPCStats, stateProvider)
	return mgr
}

// Router returns the command router for use by the evaluator.
func (m *Manager) Router() *router.Router {
	return m.router
}

// HookRegistry returns the command hook registry for constructing the hook executor.
func (m *Manager) HookRegistry() *cmdhooks.Registry {
	return m.hookRegistry
}

// OpHookRegistry returns the operation hook registry for constructing the operation hook executor.
func (m *Manager) OpHookRegistry() *ophooks.Registry {
	return m.opHookRegistry
}

// ScopeRegistry returns the scope registry for permission enforcement.
func (m *Manager) ScopeRegistry() *permissions.Registry {
	return m.scopeRegistry
}

// QueryRegistry returns the query registry for registering custom topics.
func (m *Manager) QueryRegistry() *QueryRegistry {
	return m.queryRegistry
}

// SetEventBus sets the server-wide event bus on the manager.
// The manager bridges events to plugins via GCPC when they subscribe.
func (m *Manager) SetEventBus(bus *serverEvents.Bus) {
	m.eventBus = bus
}

// SetLogCollector sets the log collector. Plugin stdout will be piped to it.
func (m *Manager) SetLogCollector(lc LogCollector) {
	m.logCollector = lc
}

// SetTracker wires an operation tracker so the manager creates a per-plugin
// lifecycle operation on each launch. Optional: when nil, plugin lifecycle
// goroutines fall back to NoCtx logging just as before.
func (m *Manager) SetTracker(t *serverOps.Tracker) {
	m.tracker = t
}

// SetClientPusher wires the connection push interface so plugins can send
// unsolicited data to client connections (e.g. Pub/Sub messages).
func (m *Manager) SetClientPusher(p ClientPusher) {
	m.clientPusher = p
}

// SetCommandMetrics wires the server-side command metrics collector into the
// manager's server-query registry and plugin-scope interest tracking.
func (m *Manager) SetCommandMetrics(c *commandmetrics.CommandCollector) {
	m.commandMetrics = c
	RegisterCommandMetricsHandlers(m.queryRegistry, c)
}

// startPluginLifecycleOp creates the lifecycle operation for a plugin instance
// and stores it on the instance. A fresh op is created on every launch (first
// launch + every restart) so each lifecycle has a distinct ID. No-op when no
// tracker is wired.
func (m *Manager) startPluginLifecycleOp(inst *PluginInstance) {
	if m.tracker == nil {
		return
	}
	op := m.tracker.Start(ops.TypePluginStart, "")
	op.Enrich(apicommand.PluginNameKey, inst.Name)
	inst.SetLifecycleOp(op)
	if m.eventBus != nil {
		m.eventBus.Emit(events.NewOperationStarted(op.ID, string(op.Type), "", op.ContextSnapshot(false)))
	}
}

// finishPluginLifecycleOp completes (or fails) the op attached to inst and
// emits the corresponding OperationComplete event. Clears the op pointer so
// a restart can attach a fresh one. Safe to call when no op is attached.
func (m *Manager) finishPluginLifecycleOp(inst *PluginInstance, failReason string) {
	op := inst.LifecycleOp()
	if op == nil {
		return
	}
	status := "completed"
	if failReason != "" {
		op.Fail(failReason)
		status = "failed"
	} else {
		op.Complete()
	}
	if m.eventBus != nil {
		m.eventBus.Emit(events.NewOperationCompleted(op.ID, string(op.Type),
			uint64(op.Duration().Nanoseconds()), status, failReason, op.ContextSnapshot(false)))
	}
	if m.tracker != nil {
		if failReason != "" {
			m.tracker.Fail(op.ID, failReason)
		} else {
			m.tracker.Complete(op.ID)
		}
	}
	inst.SetLifecycleOp(nil)
}

// pluginCtx derives a context carrying the plugin's lifecycle op (if any) from
// parentCtx. Returns parentCtx unchanged when no op is attached — downstream
// logging then naturally falls back to the parent's context (NoCtx if it has none).
func (m *Manager) pluginCtx(parentCtx context.Context, inst *PluginInstance) context.Context {
	if op := inst.LifecycleOp(); op != nil {
		return ops.WithContext(parentCtx, op)
	}
	return parentCtx
}

// EventBus returns the event bus.
func (m *Manager) EventBus() *serverEvents.Bus {
	return m.eventBus
}

// Start discovers plugins, opens the IPC listener, launches plugin processes,
// and begins accepting connections. Non-blocking: spawns goroutines and returns.
func (m *Manager) Start(parentCtx context.Context) error {
	lifecycleCtx, cancel := context.WithCancel(parentCtx)
	m.cancel = cancel

	// Discover plugins from directory + YAML overrides.
	entries, err := plugin.Discover(m.cfg)
	if err != nil {
		cancel()
		m.cancel = nil
		return fmt.Errorf("discover plugins: %w", err)
	}
	if len(entries) == 0 {
		logger.InfoNoCtx().Msg("no plugins discovered")
		return nil
	}

	// Create IPC listener.
	m.listener, err = transport.NewListener(m.cfg.SocketPath)
	if err != nil {
		cancel()
		m.cancel = nil
		return fmt.Errorf("create plugin listener: %w", err)
	}
	logger.InfoNoCtx().Str("socket", m.cfg.SocketPath).Int("plugins", len(entries)).Msg("plugin listener started")

	// Register discovered plugins and launch them.
	for _, entry := range entries {
		inst := &PluginInstance{
			Name:        entry.Name,
			BinPath:     entry.BinPath,
			Priority:    entry.Priority,
			MaxRestarts: m.cfg.MaxRestarts,
		}
		inst.setCriticalAtLoad(entry.Critical)
		inst.SetState(StateLoaded)
		m.registry.Add(inst)
		m.launchPlugin(lifecycleCtx, inst)
	}

	// Accept incoming plugin connections.
	m.wg.Add(1)
	go m.acceptLoop(lifecycleCtx)

	return nil
}

// Shutdown gracefully shuts down all plugins.
func (m *Manager) Shutdown(timeout time.Duration) {
	if m.listener == nil {
		return
	}

	logger.InfoNoCtx().Dur("timeout", timeout).Msg("shutting down plugins")

	// Close listener to stop accepting new connections.
	_ = m.listener.Close()

	deadline := time.Now().Add(timeout)

	// Send Shutdown to each running plugin.
	for _, inst := range m.registry.All() {
		st := inst.State()
		if st != StateRunning && st != StateRegistered {
			continue
		}
		if c := inst.Conn(); c != nil {
			if err := c.Send(gcpcv1.NewShutdown(deadline)); err != nil {
				logger.Warn(m.pluginCtx(context.Background(), inst)).Str("plugin", inst.Name).Err(err).Msg("failed to send shutdown")
			}
		}
	}

	// Wait for acks or timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	done := make(chan struct{})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for _, inst := range m.registry.All() {
			if c := inst.Cmd(); c != nil && c.Process != nil {
				_ = c.Wait()
			}
		}
		close(done)
	}()

	select {
	case <-done:
		logger.InfoNoCtx().Msg("all plugins shut down gracefully")
	case <-timer.C:
		// Force-kill remaining plugins.
		for _, inst := range m.registry.All() {
			if inst.State() == StateShutdown {
				continue
			}
			if c := inst.Cmd(); c != nil && c.Process != nil {
				logger.Warn(m.pluginCtx(context.Background(), inst)).Str("plugin", inst.Name).Msg("force killing plugin")
				_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			}
		}
	}

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.wg.Wait()

	// Clean up all connections and close out lifecycle ops.
	for _, inst := range m.registry.All() {
		if c := inst.Conn(); c != nil {
			_ = c.Close()
		}
		inst.SetState(StateShutdown)
		m.finishPluginLifecycleOp(inst, "")
	}
}

// launchPlugin fork/execs the plugin binary. ctx is the manager's lifecycle
// context and binds the subprocess lifetime via exec.CommandContext.
func (m *Manager) launchPlugin(ctx context.Context, inst *PluginInstance) {
	inst.SetState(StateStarting)
	// Create the lifecycle op before anything can log — later steps derive
	// their ctx from this op so every log line carries operation_id.
	m.startPluginLifecycleOp(inst)
	pluginCtx := m.pluginCtx(ctx, inst)

	cmd := exec.CommandContext(ctx, inst.BinPath)
	cmd.Env = append(os.Environ(), apiplugin.EnvSocketPath+"="+m.cfg.SocketPath)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Pipe plugin stdout to the log collector (if set), otherwise to os.Stdout.
	var logPipeR, logPipeW *os.File
	if m.logCollector != nil {
		pr, pw, err := os.Pipe()
		if err != nil {
			logger.Error(pluginCtx).Str("plugin", inst.Name).Err(err).Msg("failed to create stdout pipe")
			cmd.Stdout = os.Stdout // fallback
		} else {
			cmd.Stdout = pw
			logPipeR, logPipeW = pr, pw
		}
	} else {
		cmd.Stdout = os.Stdout
	}

	if err := cmd.Start(); err != nil {
		logger.Error(pluginCtx).Str("plugin", inst.Name).Err(err).Msg("failed to start plugin")
		// Pipe not handed to collector yet — close both ends to avoid leaking fds/goroutines.
		if logPipeW != nil {
			_ = logPipeW.Close()
			_ = logPipeR.Close()
		}
		m.finishPluginLifecycleOp(inst, "process start failed: "+err.Error())
		if inst.Critical() {
			logger.Fatal(pluginCtx).Str("plugin", inst.Name).Msg("critical plugin failed to start")
		}
		return
	}

	// Start succeeded — hand the read end to the collector and close our copy of the write end.
	if logPipeR != nil {
		m.logCollector.AddSource(inst.Name, logPipeR)
		_ = logPipeW.Close()
	}

	inst.SetCmd(cmd)
	logger.Info(pluginCtx).Str("plugin", inst.Name).Int("pid", cmd.Process.Pid).Msg("plugin process started")

	// Monitor process exit in background.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := cmd.Wait()
		if ctx.Err() != nil {
			return // shutting down, ignore
		}
		logger.Warn(m.pluginCtx(ctx, inst)).Str("plugin", inst.Name).Err(err).Msg("plugin process exited unexpectedly")
		m.handlePluginExit(ctx, inst)
	}()
}

// acceptLoop accepts incoming plugin connections and handles registration.
func (m *Manager) acceptLoop(ctx context.Context) {
	defer m.wg.Done()

	for {
		conn, err := m.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			// No lifecycle op resolvable here — accept errors happen before a
			// connection belongs to any plugin instance. Legitimately NoCtx.
			logger.ErrorNoCtx().Err(err).Msg("plugin accept error")
			continue
		}

		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.handleConnection(ctx, conn)
		}()
	}
}

// handleConnection processes the registration handshake for a new plugin connection.
func (m *Manager) handleConnection(ctx context.Context, conn *transport.Conn) {
	// Expect Register as first message.
	env, err := conn.Recv()
	if err != nil {
		// Pre-handshake — no plugin identity known yet.
		logger.ErrorNoCtx().Err(err).Msg("failed to read register message")
		_ = conn.Close()
		return
	}

	reg := env.GetRegister()
	if reg == nil {
		logger.ErrorNoCtx().Msg("first message was not Register")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "expected Register message", nil, nil))
		_ = conn.Close()
		return
	}

	// Match to known plugin.
	inst, ok := m.registry.Get(reg.Name)
	if !ok {
		logger.WarnNoCtx().Str("name", reg.Name).Msg("unknown plugin tried to register")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "unknown plugin", nil, nil))
		_ = conn.Close()
		return
	}

	// From here on we have an instance — derive a ctx that carries its
	// lifecycle op so downstream logs carry operation_id.
	pluginCtx := m.pluginCtx(ctx, inst)
	inst.SetState(StateConnected)

	// --- Scope validation ---
	grantedScopes, err := m.validateScopes(reg.Name, reg.RequestedScopes)
	if err != nil {
		logger.Error(pluginCtx).Str("plugin", reg.Name).Err(err).Msg("scope validation failed")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "scope validation failed: "+err.Error(), nil, nil))
		_ = conn.Close()
		return
	}
	m.scopeRegistry.Register(reg.Name, grantedScopes)
	m.enableCommandMetricsForPlugin(reg.Name)

	// Plugin self-describes critical, but YAML override takes precedence (already
	// seeded during Discover). Honor the plugin's value only if no YAML override.
	_, hasOverride := m.cfg.Overrides[reg.Name]
	inst.Register(conn, reg.Version, reg.Commands, scope.ScopeStrings(grantedScopes), reg.Critical, !hasOverride)

	// Register plugin commands with the router.
	if len(reg.Commands) > 0 {
		if err := m.router.RegisterPlugin(reg.Name, conn, reg.Commands); err != nil {
			logger.Error(pluginCtx).Str("plugin", reg.Name).Err(err).Msg("command registration failed")
			m.disableCommandMetricsForPlugin(reg.Name)
			m.scopeRegistry.Unregister(reg.Name)
			_ = conn.Send(gcpcv1.NewRegisterAck(false, "command registration failed: "+err.Error(), nil, nil))
			_ = conn.Close()
			return
		}
	}

	// Resolve or create the PluginConn once — reused for hooks, op-hooks,
	// event forwarding, and any future server-to-plugin async traffic.
	pc := m.router.GetPluginConn(reg.Name)
	if pc == nil && (len(reg.Hooks) > 0 || len(reg.OperationHooks) > 0) {
		pc = router.NewPluginConn(reg.Name, conn)
	}
	if pc != nil {
		m.pluginConns.Store(reg.Name, pc)
	}

	if len(reg.Hooks) > 0 {
		filteredHooks := m.filterHooksByScope(reg.Name, reg.Hooks)
		if len(filteredHooks) > 0 {
			m.hookRegistry.Register(reg.Name, int(reg.Priority), inst.Critical(), pc, filteredHooks)
		}
		if dropped := len(reg.Hooks) - len(filteredHooks); dropped > 0 {
			logger.Warn(pluginCtx).Str("plugin", reg.Name).Int("dropped", dropped).Msg("hooks dropped due to missing scope")
		}
	}

	inst.SetState(StateRegistered)

	grantedStrings := scope.ScopeStrings(grantedScopes)
	pluginCfgMap := pkgconfig.FlatPluginConfig(reg.Name)
	if err := conn.Send(gcpcv1.NewRegisterAck(true, "", grantedStrings, pluginCfgMap)); err != nil {
		logger.Error(pluginCtx).Str("plugin", reg.Name).Err(err).Msg("failed to send register ack")
		m.deregisterPlugin(reg.Name)
		_ = conn.Close()
		return
	}

	// Operation-hook registration can trigger replay through the registry's
	// onRegister callback. Register only after RegisterAck is on the wire so
	// SDK clients always observe the protocol handshake before any replayed
	// OperationHookRequest frames.
	if len(reg.OperationHooks) > 0 && m.scopeRegistry.HasScope(reg.Name, scope.ScopeOperationHook) {
		patterns := make([]string, len(reg.OperationHooks))
		for i, oh := range reg.OperationHooks {
			patterns[i] = oh.Type
		}
		// Use the priority from the first operation hook declaration (all share plugin priority).
		priority := int(reg.Priority)
		if len(reg.OperationHooks) > 0 && reg.OperationHooks[0].Priority != 0 {
			priority = int(reg.OperationHooks[0].Priority)
		}
		m.opHookRegistry.Register(reg.Name, priority, pc, patterns)
	} else if len(reg.OperationHooks) > 0 {
		logger.Warn(pluginCtx).Str("plugin", reg.Name).Int("dropped", len(reg.OperationHooks)).
			Msg("operation hooks dropped due to missing 'operation:hook' scope")
	}

	inst.SetState(StateRunning)
	pkgconfig.OnPluginReload(reg.Name, func(_ apiconfig.PluginConfig) {
		updated := pkgconfig.FlatPluginConfig(reg.Name)
		_ = conn.Send(gcpcv1.NewConfigUpdate(updated))
	})
	critical := inst.Critical()
	logger.Info(pluginCtx).Str("plugin", reg.Name).Str("version", reg.Version).Bool("critical", critical).Int("commands", len(reg.Commands)).Strs("scopes", grantedStrings).Msg("plugin registered")

	if m.eventBus != nil {
		m.eventBus.Emit(events.NewPluginRegistered(reg.Name, reg.Version, critical))
	}

	// Start health-check loop.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.healthLoop(ctx, inst)
	}()

	// Read loop for this plugin — single reader on the transport connection.
	m.readLoop(ctx, inst, pc)
}

// validateScopes resolves the granted scopes for a plugin.
// If the plugin requests scopes, they are validated against the config allowlist.
// If the plugin does not request scopes, the config allowlist (or default) is granted directly.
func (m *Manager) validateScopes(pluginName string, requested []string) ([]scope.Scope, error) {
	// Determine allowed scopes from config.
	var allowedStrings []string
	if override, ok := m.cfg.Overrides[pluginName]; ok && len(override.Scopes) > 0 {
		allowedStrings = override.Scopes
	} else {
		allowedStrings = scope.ScopeStrings(scope.DefaultScopes())
	}

	allowed, err := scope.ParseScopes(allowedStrings)
	if err != nil {
		return nil, fmt.Errorf("invalid allowed scopes in config: %w", err)
	}

	// If plugin did not request scopes, grant the full allowed set.
	if len(requested) == 0 {
		return allowed, nil
	}

	requestedScopes, err := scope.ParseScopes(requested)
	if err != nil {
		return nil, fmt.Errorf("invalid requested scopes: %w", err)
	}

	granted, denied := scope.ValidateRequest(requestedScopes, allowed)
	if len(denied) > 0 {
		logger.WarnNoCtx().Str("plugin", pluginName).Strs("denied", scope.ScopeStrings(denied)).
			Msg("some requested scopes were denied — plugin will operate with reduced capabilities")
	}

	// Always return what was granted, even if some were denied.
	// Plugins degrade gracefully at runtime when they hit a scope they don't have.
	return granted, nil
}

// filterHooksByScope returns only the hooks the plugin has scope for.
// Pre-hooks require hook:pre scope, post-hooks require hook:post scope.
func (m *Manager) filterHooksByScope(pluginName string, hooks []*gcpcv1.HookDeclV1) []*gcpcv1.HookDeclV1 {
	hasPre := m.scopeRegistry.HasScope(pluginName, scope.ScopeHookPre)
	hasPost := m.scopeRegistry.HasScope(pluginName, scope.ScopeHookPost)

	var filtered []*gcpcv1.HookDeclV1
	for _, h := range hooks {
		switch h.Phase {
		case gcpcv1.HookPhaseV1_HOOK_PHASE_PRE:
			if hasPre {
				filtered = append(filtered, h)
			}
		case gcpcv1.HookPhaseV1_HOOK_PHASE_POST:
			if hasPost {
				filtered = append(filtered, h)
			}
		}
	}
	return filtered
}

// healthLoop periodically sends health checks to a plugin.
func (m *Manager) healthLoop(ctx context.Context, inst *PluginInstance) {
	ticker := time.NewTicker(m.cfg.HealthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if inst.State() != StateRunning {
				return
			}
			c := inst.Conn()
			if c == nil {
				return
			}
			if err := c.Send(gcpcv1.NewHealthCheck()); err != nil {
				logger.Warn(m.pluginCtx(ctx, inst)).Str("plugin", inst.Name).Err(err).Msg("health check send failed")
				inst.SetState(StateUnhealthy)
				return
			}
		}
	}
}

// readLoop reads messages from a connected plugin. It is the single reader
// on the transport connection — response types (CommandResponse, HookResponse,
// OperationHookResponse) are dispatched to the PluginConn's pending channels,
// while management messages (ClientPush, EventSubscribe, etc.) are handled
// directly.
func (m *Manager) readLoop(ctx context.Context, inst *PluginInstance, pc *router.PluginConn) {
	conn := inst.Conn()
	if conn == nil {
		return
	}
	loopCtx := m.pluginCtx(ctx, inst)
	for {
		env, err := conn.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			if inst.State() == StateRunning {
				logger.Warn(loopCtx).Str("plugin", inst.Name).Err(err).Msg("plugin connection lost")
				inst.SetState(StateUnhealthy)
			}
			return
		}

		switch env.Payload.(type) {
		case *gcpcv1.EnvelopeV1_HealthResponse:
			resp := env.GetHealthResponse()
			if resp.Ok {
				inst.SetLastHealth(time.Now())
			} else {
				logger.Warn(loopCtx).Str("plugin", inst.Name).Str("status", resp.Status).Msg("plugin reported unhealthy")
				inst.SetState(StateUnhealthy)
				return
			}
		case *gcpcv1.EnvelopeV1_ShutdownAck:
			logger.Info(loopCtx).Str("plugin", inst.Name).Msg("plugin acknowledged shutdown")
			inst.SetState(StateShutdown)
			return
		case *gcpcv1.EnvelopeV1_EventSubscribe:
			sub := env.GetEventSubscribe()
			if !m.scopeRegistry.HasScope(inst.Name, scope.ScopeEvents) {
				logger.Warn(loopCtx).Str("plugin", inst.Name).Msg("event subscription denied: missing 'events' scope")
				continue
			}
			if m.eventBus == nil {
				logger.Warn(loopCtx).Str("plugin", inst.Name).Msg("event subscription failed: event bus not set")
				continue
			}
			if pc == nil {
				pc = router.NewPluginConn(inst.Name, conn)
				m.pluginConns.Store(inst.Name, pc)
			}
			types := make([]events.Type, len(sub.Types))
			for i, t := range sub.Types {
				types[i] = events.Type(t)
			}
			// Bridge: subscribe on the server bus with a handler that forwards via
			// the per-plugin FIFO writer. Event handlers must not block the event
			// bus on plugin reads; overloaded plugins drop best-effort telemetry.
			pluginConn := pc
			pluginName := inst.Name
			bridgeMode := benchEventBridgeMode()
			if bridgeMode == eventBridgeModeBridgeOff {
				logger.Warn(loopCtx).Str("plugin", inst.Name).Msg("benchmark event bridge mode active: dropping events before IPC enqueue")
			}
			m.eventBus.Subscribe("plugin:"+inst.Name, types, func(evt events.Event) {
				if bridgeMode == eventBridgeModeBridgeOff {
					return
				}
				evtProto := evt.Proto
				pluginConn.SendFireAndForgetLazy(func() *gcpcv1.EnvelopeV1 {
					projected := projectEventForPlugin(evtProto, pluginName)
					return &gcpcv1.EnvelopeV1{
						Version: gcpcv1.ProtocolVersion,
						Payload: &gcpcv1.EnvelopeV1_Event{Event: projected},
					}
				})
			})
		case *gcpcv1.EnvelopeV1_ServerQuery:
			query := env.GetServerQuery()
			requiredScope := scope.ScopeForTopic(query.Topic)
			if !m.scopeRegistry.HasScope(inst.Name, requiredScope) {
				_ = conn.Send(gcpcv1.NewServerQueryResponse(query.RequestId, nil,
					fmt.Sprintf("permission denied: missing scope %q", requiredScope)))
				continue
			}
			data, qErr := m.queryRegistry.Handle(query.Topic, query.Params)
			errMsg := ""
			if qErr != nil {
				errMsg = qErr.Error()
			}
			_ = conn.Send(gcpcv1.NewServerQueryResponse(query.RequestId, data, errMsg))
		case *gcpcv1.EnvelopeV1_ClientPush:
			push := env.GetClientPush()
			if m.clientPusher == nil {
				logger.Warn(loopCtx).Str("plugin", inst.Name).Msg("client push received but no pusher configured")
				continue
			}
			if !m.scopeRegistry.HasScope(inst.Name, scope.ScopeWrite) {
				logger.Warn(loopCtx).Str("plugin", inst.Name).Msg("client push denied: missing 'write' scope")
				continue
			}
			if err := m.clientPusher.Push(push.ConnectionId, push.Data); err != nil {
				logger.Debug(loopCtx).Str("plugin", inst.Name).Err(err).Msg("client push failed")
			}
		case *gcpcv1.EnvelopeV1_CommandResponse:
			if pc != nil {
				pc.Deliver(env.GetCommandResponse().RequestId, env)
			}
		case *gcpcv1.EnvelopeV1_HookResponse:
			if pc != nil {
				pc.Deliver(env.GetHookResponse().RequestId, env)
			}
		case *gcpcv1.EnvelopeV1_OperationHookResponse:
			if pc != nil {
				pc.Deliver(env.GetOperationHookResponse().RequestId, env)
			}
		default:
			logger.Debug(loopCtx).Str("plugin", inst.Name).Msg("unexpected message from plugin")
		}
	}
}

// deregisterPlugin removes every registration this plugin held across the
// subsystems. Idempotent — each individual Unregister is safe to call for a
// name that never registered. Callers must still manage instance state
// (SetState, emit events) separately.
func (m *Manager) deregisterPlugin(name string) {
	m.disableCommandMetricsForPlugin(name)
	m.router.UnregisterPlugin(name)
	m.hookRegistry.Unregister(name)
	m.opHookRegistry.Unregister(name)
	m.scopeRegistry.Unregister(name)
	if pc, ok := m.pluginConns.LoadAndDelete(name); ok {
		pc.(*router.PluginConn).Close()
	}
	if m.eventBus != nil {
		m.eventBus.Unsubscribe("plugin:" + name)
	}
}

func (m *Manager) enableCommandMetricsForPlugin(name string) {
	if m.commandMetrics == nil || !m.scopeRegistry.HasScope(name, scope.ScopeForTopic(commandmetrics.CommandsTopic)) {
		return
	}
	if _, loaded := m.commandMetricsConsumers.LoadOrStore(name, struct{}{}); !loaded {
		m.commandMetrics.AddConsumer()
	}
}

func (m *Manager) disableCommandMetricsForPlugin(name string) {
	if m.commandMetrics == nil {
		return
	}
	if _, loaded := m.commandMetricsConsumers.LoadAndDelete(name); loaded {
		m.commandMetrics.RemoveConsumer()
	}
}

// handlePluginExit handles unexpected plugin process termination.
func (m *Manager) handlePluginExit(ctx context.Context, inst *PluginInstance) {
	exitCtx := m.pluginCtx(ctx, inst)
	m.deregisterPlugin(inst.Name)
	critical := inst.Critical()
	if m.eventBus != nil {
		m.eventBus.Emit(events.NewPluginCrashed(inst.Name, critical, "process exited unexpectedly"))
	}

	// Close the current lifecycle op before deciding what to do next —
	// a subsequent relaunch will start a fresh op.
	m.finishPluginLifecycleOp(inst, "process exited unexpectedly")

	if critical {
		logger.Fatal(exitCtx).Str("plugin", inst.Name).Msg("critical plugin crashed — shutting down server")
		return
	}

	if restarts := inst.Restarts(); restarts >= inst.MaxRestarts {
		logger.Error(exitCtx).Str("plugin", inst.Name).Int("restarts", restarts).Msg("max restarts exceeded, giving up")
		inst.SetState(StateShutdown)
		return
	}

	attempt := inst.IncrementRestarts()
	logger.Info(exitCtx).Str("plugin", inst.Name).Int("attempt", attempt).Msg("restarting non-critical plugin")
	inst.SetState(StateRestarting)

	if c := inst.Conn(); c != nil {
		_ = c.Close()
		inst.SetConn(nil)
	}

	m.launchPlugin(ctx, inst)
}

// projectEventForPlugin builds the per-plugin event view without a full proto clone.
// The event envelope and context-bearing payload are copied before filtering so
// visibility rules never mutate the server-owned event or another plugin's view.
func projectEventForPlugin(evt *gcpcv1.EventV1, pluginName string) *gcpcv1.EventV1 {
	if evt == nil {
		return nil
	}
	projected := &gcpcv1.EventV1{
		Type:        evt.Type,
		Timestamp:   evt.Timestamp,
		OperationId: evt.OperationId,
		Data:        evt.Data,
	}
	switch d := evt.Data.(type) {
	case *gcpcv1.EventV1_OperationStart:
		if d.OperationStart != nil {
			payload := d.OperationStart
			projected.Data = &gcpcv1.EventV1_OperationStart{OperationStart: &gcpcv1.OperationStartEventV1{
				Id:       payload.Id,
				Type:     payload.Type,
				ParentId: payload.ParentId,
				Context:  opctx.FilterForPlugin(payload.Context, pluginName),
			}}
		}
	case *gcpcv1.EventV1_OperationComplete:
		if d.OperationComplete != nil {
			payload := d.OperationComplete
			projected.Data = &gcpcv1.EventV1_OperationComplete{OperationComplete: &gcpcv1.OperationCompleteEventV1{
				Id:         payload.Id,
				Type:       payload.Type,
				ElapsedNs:  payload.ElapsedNs,
				Status:     payload.Status,
				FailReason: payload.FailReason,
				Context:    opctx.FilterForPlugin(payload.Context, pluginName),
			}}
		}
	case *gcpcv1.EventV1_CommandPost:
		if d.CommandPost != nil {
			payload := d.CommandPost
			projected.Data = &gcpcv1.EventV1_CommandPost{CommandPost: &gcpcv1.CommandPostEventV1{
				Command:   payload.Command,
				Args:      payload.Args,
				ElapsedNs: payload.ElapsedNs,
				Result:    payload.Result,
				Error:     payload.Error,
				Metadata:  opctx.FilterForPlugin(payload.Metadata, pluginName),
			}}
		}
	case *gcpcv1.EventV1_CommandPre:
		if d.CommandPre != nil {
			payload := d.CommandPre
			projected.Data = &gcpcv1.EventV1_CommandPre{CommandPre: &gcpcv1.CommandPreEventV1{
				Command:  payload.Command,
				Args:     payload.Args,
				Metadata: opctx.FilterForPlugin(payload.Metadata, pluginName),
			}}
		}
	case *gcpcv1.EventV1_LogEntry:
		if d.LogEntry != nil {
			payload := d.LogEntry
			projected.Data = &gcpcv1.EventV1_LogEntry{LogEntry: &gcpcv1.LogEntryEventV1{
				Level:   payload.Level,
				Message: payload.Message,
				Caller:  payload.Caller,
				Fields:  opctx.FilterForPlugin(payload.Fields, pluginName),
			}}
		}
	}
	return projected
}
