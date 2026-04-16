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

	opctx "gocache/api/context"
	"gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"
	"gocache/api/logger"
	"gocache/api/transport"
	serverEvents "gocache/pkg/events"
	"gocache/pkg/plugin"
	"gocache/pkg/plugin/cmdhooks"
	"gocache/pkg/plugin/ophooks"
	"gocache/pkg/plugin/permissions"
	"gocache/pkg/plugin/router"

	"google.golang.org/protobuf/proto"
)

// LogCollector is the interface for adding log sources.
// Defined here to avoid importing pkg/logcollector from the manager.
type LogCollector interface {
	AddSource(name string, r io.Reader)
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

	// cancel terminates the lifecycle context derived inside Start.
	// nil before Start; reset to nil by Shutdown.
	cancel context.CancelFunc

	wg sync.WaitGroup
}

// NewManager creates a plugin manager with the given configuration.
// coreCommands is the list of command names handled by the core evaluator;
// plugin commands that shadow these will be rejected during registration.
func NewManager(cfg plugin.PluginsConfig, coreCommands []string, stateProvider ServerStateProvider) *Manager {
	reg := NewRegistry()
	qr := NewQueryRegistry()
	RegisterBuiltinHandlers(qr, reg, stateProvider)
	return &Manager{
		cfg:            cfg,
		registry:       reg,
		router:         router.NewRouter(coreCommands),
		hookRegistry:   cmdhooks.NewRegistry(),
		opHookRegistry: ophooks.NewRegistry(),
		scopeRegistry:  permissions.NewRegistry(),
		queryRegistry:  qr,
	}
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
				logger.WarnNoCtx().Str("plugin", inst.Name).Err(err).Msg("failed to send shutdown")
			}
		}
	}

	// Wait for acks or timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	done := make(chan struct{})
	go func() {
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
				logger.WarnNoCtx().Str("plugin", inst.Name).Msg("force killing plugin")
				_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			}
		}
	}

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.wg.Wait()

	// Clean up all connections.
	for _, inst := range m.registry.All() {
		if c := inst.Conn(); c != nil {
			_ = c.Close()
		}
		inst.SetState(StateShutdown)
	}
}

// launchPlugin fork/execs the plugin binary. ctx is the manager's lifecycle
// context and binds the subprocess lifetime via exec.CommandContext.
func (m *Manager) launchPlugin(ctx context.Context, inst *PluginInstance) {
	inst.SetState(StateStarting)

	cmd := exec.CommandContext(ctx, inst.BinPath)
	cmd.Env = append(os.Environ(), "GOCACHE_PLUGIN_SOCK="+m.cfg.SocketPath)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Pipe plugin stdout to the log collector (if set), otherwise to os.Stdout.
	var logPipeR, logPipeW *os.File
	if m.logCollector != nil {
		pr, pw, err := os.Pipe()
		if err != nil {
			logger.ErrorNoCtx().Str("plugin", inst.Name).Err(err).Msg("failed to create stdout pipe")
			cmd.Stdout = os.Stdout // fallback
		} else {
			cmd.Stdout = pw
			logPipeR, logPipeW = pr, pw
		}
	} else {
		cmd.Stdout = os.Stdout
	}

	if err := cmd.Start(); err != nil {
		logger.ErrorNoCtx().Str("plugin", inst.Name).Err(err).Msg("failed to start plugin")
		// Pipe not handed to collector yet — close both ends to avoid leaking fds/goroutines.
		if logPipeW != nil {
			_ = logPipeW.Close()
			_ = logPipeR.Close()
		}
		if inst.Critical() {
			logger.FatalNoCtx().Str("plugin", inst.Name).Msg("critical plugin failed to start")
		}
		return
	}

	// Start succeeded — hand the read end to the collector and close our copy of the write end.
	if logPipeR != nil {
		m.logCollector.AddSource(inst.Name, logPipeR)
		_ = logPipeW.Close()
	}

	inst.SetCmd(cmd)
	logger.InfoNoCtx().Str("plugin", inst.Name).Int("pid", cmd.Process.Pid).Msg("plugin process started")

	// Monitor process exit in background.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := cmd.Wait()
		if ctx.Err() != nil {
			return // shutting down, ignore
		}
		logger.WarnNoCtx().Str("plugin", inst.Name).Err(err).Msg("plugin process exited unexpectedly")
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
		logger.ErrorNoCtx().Err(err).Msg("failed to read register message")
		_ = conn.Close()
		return
	}

	reg := env.GetRegister()
	if reg == nil {
		logger.ErrorNoCtx().Msg("first message was not Register")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "expected Register message", nil))
		_ = conn.Close()
		return
	}

	// Match to known plugin.
	inst, ok := m.registry.Get(reg.Name)
	if !ok {
		logger.WarnNoCtx().Str("name", reg.Name).Msg("unknown plugin tried to register")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "unknown plugin", nil))
		_ = conn.Close()
		return
	}

	// --- Scope validation ---
	grantedScopes, err := m.validateScopes(reg.Name, reg.RequestedScopes)
	if err != nil {
		logger.ErrorNoCtx().Str("plugin", reg.Name).Err(err).Msg("scope validation failed")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "scope validation failed: "+err.Error(), nil))
		_ = conn.Close()
		return
	}
	m.scopeRegistry.Register(reg.Name, grantedScopes)

	// Plugin self-describes critical, but YAML override takes precedence (already
	// seeded during Discover). Honor the plugin's value only if no YAML override.
	_, hasOverride := m.cfg.Overrides[reg.Name]
	inst.Register(conn, reg.Version, reg.Commands, permissions.ScopeStrings(grantedScopes), reg.Critical, !hasOverride)

	// Register plugin commands with the router.
	if len(reg.Commands) > 0 {
		if err := m.router.RegisterPlugin(reg.Name, conn, reg.Commands); err != nil {
			logger.ErrorNoCtx().Str("plugin", reg.Name).Err(err).Msg("command registration failed")
			m.scopeRegistry.Unregister(reg.Name)
			_ = conn.Send(gcpcv1.NewRegisterAck(false, "command registration failed: "+err.Error(), nil))
			_ = conn.Close()
			return
		}
	}

	// Register hooks, filtered by scope. Only register hooks the plugin has scope for.
	if len(reg.Hooks) > 0 {
		filteredHooks := m.filterHooksByScope(reg.Name, reg.Hooks)
		if len(filteredHooks) > 0 {
			pc := m.router.GetPluginConn(reg.Name)
			if pc == nil {
				// Hook-only plugin — create a PluginConn for it.
				pc = router.NewPluginConn(reg.Name, conn)
			}
			m.hookRegistry.Register(reg.Name, int(reg.Priority), inst.Critical(), pc, filteredHooks)
		}
		if dropped := len(reg.Hooks) - len(filteredHooks); dropped > 0 {
			logger.WarnNoCtx().Str("plugin", reg.Name).Int("dropped", dropped).Msg("hooks dropped due to missing scope")
		}
	}

	// Register operation hooks if the plugin has the operation:hook scope.
	if len(reg.OperationHooks) > 0 && m.scopeRegistry.HasScope(reg.Name, permissions.ScopeOperationHook) {
		pc := m.router.GetPluginConn(reg.Name)
		if pc == nil {
			pc = router.NewPluginConn(reg.Name, conn)
		}
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
		logger.WarnNoCtx().Str("plugin", reg.Name).Int("dropped", len(reg.OperationHooks)).
			Msg("operation hooks dropped due to missing 'operation:hook' scope")
	}

	inst.SetState(StateRegistered)

	grantedStrings := permissions.ScopeStrings(grantedScopes)
	if err := conn.Send(gcpcv1.NewRegisterAck(true, "", grantedStrings)); err != nil {
		logger.ErrorNoCtx().Str("plugin", reg.Name).Err(err).Msg("failed to send register ack")
		m.router.UnregisterPlugin(reg.Name)
		m.scopeRegistry.Unregister(reg.Name)
		_ = conn.Close()
		return
	}

	inst.SetState(StateRunning)
	critical := inst.Critical()
	logger.InfoNoCtx().Str("plugin", reg.Name).Str("version", reg.Version).Bool("critical", critical).Int("commands", len(reg.Commands)).Strs("scopes", grantedStrings).Msg("plugin registered")

	if m.eventBus != nil {
		m.eventBus.Emit(events.NewPluginRegistered(reg.Name, reg.Version, critical))
	}

	// Start health-check loop.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.healthLoop(ctx, inst)
	}()

	// Read loop for this plugin (handles ShutdownAck and future messages).
	m.readLoop(ctx, inst)
}

// validateScopes resolves the granted scopes for a plugin.
// If the plugin requests scopes, they are validated against the config allowlist.
// If the plugin does not request scopes, the config allowlist (or default) is granted directly.
func (m *Manager) validateScopes(pluginName string, requested []string) ([]permissions.Scope, error) {
	// Determine allowed scopes from config.
	var allowedStrings []string
	if override, ok := m.cfg.Overrides[pluginName]; ok && len(override.Scopes) > 0 {
		allowedStrings = override.Scopes
	} else {
		allowedStrings = permissions.ScopeStrings(permissions.DefaultScopes())
	}

	allowed, err := permissions.ParseScopes(allowedStrings)
	if err != nil {
		return nil, fmt.Errorf("invalid allowed scopes in config: %w", err)
	}

	// If plugin did not request scopes, grant the full allowed set.
	if len(requested) == 0 {
		return allowed, nil
	}

	requestedScopes, err := permissions.ParseScopes(requested)
	if err != nil {
		return nil, fmt.Errorf("invalid requested scopes: %w", err)
	}

	granted, denied := permissions.ValidateRequest(requestedScopes, allowed)
	if len(denied) > 0 {
		logger.WarnNoCtx().Str("plugin", pluginName).Strs("denied", permissions.ScopeStrings(denied)).
			Msg("some requested scopes were denied — plugin will operate with reduced capabilities")
	}

	// Always return what was granted, even if some were denied.
	// Plugins degrade gracefully at runtime when they hit a scope they don't have.
	if len(granted) == 0 {
		// Grant at least the defaults so the plugin can function minimally.
		return allowed, nil
	}
	return granted, nil
}

// filterHooksByScope returns only the hooks the plugin has scope for.
// Pre-hooks require hook:pre scope, post-hooks require hook:post scope.
func (m *Manager) filterHooksByScope(pluginName string, hooks []*gcpcv1.HookDeclV1) []*gcpcv1.HookDeclV1 {
	hasPre := m.scopeRegistry.HasScope(pluginName, permissions.ScopeHookPre)
	hasPost := m.scopeRegistry.HasScope(pluginName, permissions.ScopeHookPost)

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
				logger.WarnNoCtx().Str("plugin", inst.Name).Err(err).Msg("health check send failed")
				inst.SetState(StateUnhealthy)
				return
			}
		}
	}
}

// readLoop reads messages from a connected plugin.
func (m *Manager) readLoop(ctx context.Context, inst *PluginInstance) {
	conn := inst.Conn()
	if conn == nil {
		return
	}
	for {
		env, err := conn.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			if inst.State() == StateRunning {
				logger.WarnNoCtx().Str("plugin", inst.Name).Err(err).Msg("plugin connection lost")
				m.router.UnregisterPlugin(inst.Name)
				m.hookRegistry.Unregister(inst.Name)
				m.opHookRegistry.Unregister(inst.Name)
				m.scopeRegistry.Unregister(inst.Name)
				if m.eventBus != nil {
					m.eventBus.Unsubscribe("plugin:" + inst.Name)
				}
				inst.SetState(StateUnhealthy)
				if m.eventBus != nil {
					m.eventBus.Emit(events.NewPluginCrashed(inst.Name, inst.Critical(), err.Error()))
				}
			}
			return
		}

		switch env.Payload.(type) {
		case *gcpcv1.EnvelopeV1_HealthResponse:
			resp := env.GetHealthResponse()
			if resp.Ok {
				inst.SetLastHealth(time.Now())
			} else {
				logger.WarnNoCtx().Str("plugin", inst.Name).Str("status", resp.Status).Msg("plugin reported unhealthy")
				inst.SetState(StateUnhealthy)
				return
			}
		case *gcpcv1.EnvelopeV1_ShutdownAck:
			logger.InfoNoCtx().Str("plugin", inst.Name).Msg("plugin acknowledged shutdown")
			inst.SetState(StateShutdown)
			return
		case *gcpcv1.EnvelopeV1_EventSubscribe:
			sub := env.GetEventSubscribe()
			if !m.scopeRegistry.HasScope(inst.Name, permissions.ScopeEvents) {
				logger.WarnNoCtx().Str("plugin", inst.Name).Msg("event subscription denied: missing 'events' scope")
				continue
			}
			if m.eventBus == nil {
				logger.WarnNoCtx().Str("plugin", inst.Name).Msg("event subscription failed: event bus not set")
				continue
			}
			types := make([]events.Type, len(sub.Types))
			for i, t := range sub.Types {
				types[i] = events.Type(t)
			}
			// Bridge: subscribe on the server bus with a handler that forwards via GCPC.
			// Context in events is filtered per plugin visibility before forwarding.
			pluginConn := conn
			pluginName := inst.Name
			m.eventBus.Subscribe("plugin:"+inst.Name, types, func(evt events.Event) {
				cloned := proto.Clone(evt.Proto).(*gcpcv1.EventV1)
				filterEventContext(cloned, pluginName)
				gcpcEnv := &gcpcv1.EnvelopeV1{
					Version: gcpcv1.ProtocolVersion,
					Payload: &gcpcv1.EnvelopeV1_Event{Event: cloned},
				}
				_ = pluginConn.Send(gcpcEnv)
			})
		case *gcpcv1.EnvelopeV1_ServerQuery:
			query := env.GetServerQuery()
			requiredScope := permissions.ScopeForTopic(query.Topic)
			if !m.scopeRegistry.HasScope(inst.Name, requiredScope) {
				_ = conn.Send(gcpcv1.NewServerQueryResponse(query.RequestId, nil,
					fmt.Sprintf("permission denied: missing scope %q", requiredScope)))
				continue
			}
			data, qErr := m.queryRegistry.Handle(query.Topic)
			errMsg := ""
			if qErr != nil {
				errMsg = qErr.Error()
			}
			_ = conn.Send(gcpcv1.NewServerQueryResponse(query.RequestId, data, errMsg))
		default:
			logger.DebugNoCtx().Str("plugin", inst.Name).Msg("unexpected message from plugin")
		}
	}
}

// handlePluginExit handles unexpected plugin process termination.
func (m *Manager) handlePluginExit(ctx context.Context, inst *PluginInstance) {
	// Unregister commands, hooks, scopes, and event subscriptions.
	m.router.UnregisterPlugin(inst.Name)
	m.hookRegistry.Unregister(inst.Name)
	m.opHookRegistry.Unregister(inst.Name)
	m.scopeRegistry.Unregister(inst.Name)
	critical := inst.Critical()
	if m.eventBus != nil {
		m.eventBus.Unsubscribe("plugin:" + inst.Name)
		m.eventBus.Emit(events.NewPluginCrashed(inst.Name, critical, "process exited unexpectedly"))
	}

	if critical {
		logger.FatalNoCtx().Str("plugin", inst.Name).Msg("critical plugin crashed — shutting down server")
		return
	}

	if restarts := inst.Restarts(); restarts >= inst.MaxRestarts {
		logger.ErrorNoCtx().Str("plugin", inst.Name).Int("restarts", restarts).Msg("max restarts exceeded, giving up")
		inst.SetState(StateShutdown)
		return
	}

	attempt := inst.IncrementRestarts()
	logger.InfoNoCtx().Str("plugin", inst.Name).Int("attempt", attempt).Msg("restarting non-critical plugin")
	inst.SetState(StateRestarting)

	if c := inst.Conn(); c != nil {
		_ = c.Close()
		inst.SetConn(nil)
	}

	m.launchPlugin(ctx, inst)
}

// filterEventContext filters context maps in event data per plugin visibility.
// Events carrying context (operation start/complete, command post, log entry) have
// their context filtered so plugins only see _*, shared.*, and their own namespace.
func filterEventContext(evt *gcpcv1.EventV1, pluginName string) {
	switch d := evt.Data.(type) {
	case *gcpcv1.EventV1_OperationStart:
		if d.OperationStart != nil {
			d.OperationStart.Context = opctx.FilterForPlugin(d.OperationStart.Context, pluginName)
		}
	case *gcpcv1.EventV1_OperationComplete:
		if d.OperationComplete != nil {
			d.OperationComplete.Context = opctx.FilterForPlugin(d.OperationComplete.Context, pluginName)
		}
	case *gcpcv1.EventV1_CommandPost:
		if d.CommandPost != nil {
			d.CommandPost.Metadata = opctx.FilterForPlugin(d.CommandPost.Metadata, pluginName)
		}
	case *gcpcv1.EventV1_CommandPre:
		if d.CommandPre != nil {
			d.CommandPre.Metadata = opctx.FilterForPlugin(d.CommandPre.Metadata, pluginName)
		}
	case *gcpcv1.EventV1_LogEntry:
		if d.LogEntry != nil {
			d.LogEntry.Fields = opctx.FilterForPlugin(d.LogEntry.Fields, pluginName)
		}
	}
}
