package manager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"gocache/api/events"
	gcpcv1 "gocache/api/gcpc/v1"
	"gocache/api/transport"
	serverEvents "gocache/pkg/events"
	"gocache/pkg/logger"
	"gocache/pkg/plugin"
	"gocache/pkg/plugin/hooks"
	"gocache/pkg/plugin/permissions"
	"gocache/pkg/plugin/router"

	"google.golang.org/protobuf/proto"
)

// Manager handles plugin lifecycle: discovery, fork/exec, registration,
// health monitoring, restart, and graceful shutdown.
type Manager struct {
	cfg           plugin.PluginsConfig
	listener      *transport.Listener
	registry      *Registry
	router        *router.Router
	hookRegistry  *hooks.Registry
	scopeRegistry *permissions.Registry
	queryRegistry *QueryRegistry
	eventBus      *serverEvents.Bus
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

// NewManager creates a plugin manager with the given configuration.
// coreCommands is the list of command names handled by the core evaluator;
// plugin commands that shadow these will be rejected during registration.
func NewManager(cfg plugin.PluginsConfig, coreCommands []string, stateProvider ServerStateProvider) *Manager {
	reg := NewRegistry()
	qr := NewQueryRegistry()
	RegisterBuiltinHandlers(qr, reg, stateProvider)
	return &Manager{
		cfg:           cfg,
		registry:      reg,
		router:        router.NewRouter(coreCommands),
		hookRegistry:  hooks.NewRegistry(),
		scopeRegistry: permissions.NewRegistry(),
		queryRegistry: qr,
	}
}

// Router returns the command router for use by the evaluator.
func (m *Manager) Router() *router.Router {
	return m.router
}

// HookRegistry returns the hook registry for constructing the hook executor.
func (m *Manager) HookRegistry() *hooks.Registry {
	return m.hookRegistry
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

// EventBus returns the event bus.
func (m *Manager) EventBus() *serverEvents.Bus {
	return m.eventBus
}

// Start discovers plugins, opens the IPC listener, launches plugin processes,
// and begins accepting connections. Non-blocking: spawns goroutines and returns.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)

	// Discover plugins from directory + YAML overrides.
	entries, err := plugin.Discover(m.cfg)
	if err != nil {
		return fmt.Errorf("discover plugins: %w", err)
	}
	if len(entries) == 0 {
		logger.Info().Msg("no plugins discovered")
		return nil
	}

	// Create IPC listener.
	m.listener, err = transport.NewListener(m.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("create plugin listener: %w", err)
	}
	logger.Info().Str("socket", m.cfg.SocketPath).Int("plugins", len(entries)).Msg("plugin listener started")

	// Register discovered plugins and launch them.
	for _, entry := range entries {
		inst := &PluginInstance{
			Name:        entry.Name,
			BinPath:     entry.BinPath,
			Critical:    entry.Critical,
			Priority:    entry.Priority,
			State:       StateLoaded,
			MaxRestarts: m.cfg.MaxRestarts,
		}
		m.registry.Add(inst)
		m.launchPlugin(inst)
	}

	// Accept incoming plugin connections.
	m.wg.Add(1)
	go m.acceptLoop()

	return nil
}

// Shutdown gracefully shuts down all plugins.
func (m *Manager) Shutdown(timeout time.Duration) {
	if m.listener == nil {
		return
	}

	logger.Info().Dur("timeout", timeout).Msg("shutting down plugins")

	// Close listener to stop accepting new connections.
	_ = m.listener.Close()

	deadline := time.Now().Add(timeout)

	// Send Shutdown to each running plugin.
	for _, inst := range m.registry.All() {
		if inst.State != StateRunning && inst.State != StateRegistered {
			continue
		}
		if inst.Conn != nil {
			if err := inst.Conn.Send(gcpcv1.NewShutdown(deadline)); err != nil {
				logger.Warn().Str("plugin", inst.Name).Err(err).Msg("failed to send shutdown")
			}
		}
	}

	// Wait for acks or timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	done := make(chan struct{})
	go func() {
		for _, inst := range m.registry.All() {
			if inst.Cmd != nil && inst.Cmd.Process != nil {
				_ = inst.Cmd.Wait()
			}
		}
		close(done)
	}()

	select {
	case <-done:
		logger.Info().Msg("all plugins shut down gracefully")
	case <-timer.C:
		// Force-kill remaining plugins.
		for _, inst := range m.registry.All() {
			if inst.State == StateShutdown {
				continue
			}
			if inst.Cmd != nil && inst.Cmd.Process != nil {
				logger.Warn().Str("plugin", inst.Name).Msg("force killing plugin")
				_ = syscall.Kill(-inst.Cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}

	m.cancel()
	m.wg.Wait()

	// Clean up all connections.
	for _, inst := range m.registry.All() {
		if inst.Conn != nil {
			_ = inst.Conn.Close()
		}
		m.registry.SetState(inst.Name, StateShutdown)
	}
}

// launchPlugin fork/execs the plugin binary.
func (m *Manager) launchPlugin(inst *PluginInstance) {
	m.registry.SetState(inst.Name, StateStarting)

	cmd := exec.CommandContext(m.ctx, inst.BinPath)
	cmd.Env = append(os.Environ(), "GOCACHE_PLUGIN_SOCK="+m.cfg.SocketPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logger.Error().Str("plugin", inst.Name).Err(err).Msg("failed to start plugin")
		if inst.Critical {
			logger.Fatal().Str("plugin", inst.Name).Msg("critical plugin failed to start")
		}
		return
	}

	inst.Cmd = cmd
	logger.Info().Str("plugin", inst.Name).Int("pid", cmd.Process.Pid).Msg("plugin process started")

	// Monitor process exit in background.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := cmd.Wait()
		if m.ctx.Err() != nil {
			return // shutting down, ignore
		}
		logger.Warn().Str("plugin", inst.Name).Err(err).Msg("plugin process exited unexpectedly")
		m.handlePluginExit(inst)
	}()
}

// acceptLoop accepts incoming plugin connections and handles registration.
func (m *Manager) acceptLoop() {
	defer m.wg.Done()

	for {
		conn, err := m.listener.Accept()
		if err != nil {
			if m.ctx.Err() != nil {
				return // shutting down
			}
			logger.Error().Err(err).Msg("plugin accept error")
			continue
		}

		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.handleConnection(conn)
		}()
	}
}

// handleConnection processes the registration handshake for a new plugin connection.
func (m *Manager) handleConnection(conn *transport.Conn) {
	// Expect Register as first message.
	env, err := conn.Recv()
	if err != nil {
		logger.Error().Err(err).Msg("failed to read register message")
		_ = conn.Close()
		return
	}

	reg := env.GetRegister()
	if reg == nil {
		logger.Error().Msg("first message was not Register")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "expected Register message", nil))
		_ = conn.Close()
		return
	}

	// Match to known plugin.
	inst, ok := m.registry.Get(reg.Name)
	if !ok {
		logger.Warn().Str("name", reg.Name).Msg("unknown plugin tried to register")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "unknown plugin", nil))
		_ = conn.Close()
		return
	}

	// Accept registration.
	inst.Conn = conn
	inst.Version = reg.Version
	inst.Commands = reg.Commands
	// Plugin self-describes critical, but YAML override takes precedence (already set in Discover).
	// Only apply plugin's self-description if not overridden.
	if _, hasOverride := m.cfg.Overrides[reg.Name]; !hasOverride {
		inst.Critical = reg.Critical
	}

	// --- Scope validation ---
	grantedScopes, err := m.validateScopes(reg.Name, reg.RequestedScopes)
	if err != nil {
		logger.Error().Str("plugin", reg.Name).Err(err).Msg("scope validation failed")
		_ = conn.Send(gcpcv1.NewRegisterAck(false, "scope validation failed: "+err.Error(), nil))
		_ = conn.Close()
		return
	}
	m.scopeRegistry.Register(reg.Name, grantedScopes)
	inst.GrantedScopes = permissions.ScopeStrings(grantedScopes)

	// Register plugin commands with the router.
	if len(reg.Commands) > 0 {
		if err := m.router.RegisterPlugin(reg.Name, conn, reg.Commands); err != nil {
			logger.Error().Str("plugin", reg.Name).Err(err).Msg("command registration failed")
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
			m.hookRegistry.Register(reg.Name, int(reg.Priority), inst.Critical, pc, filteredHooks)
		}
		if dropped := len(reg.Hooks) - len(filteredHooks); dropped > 0 {
			logger.Warn().Str("plugin", reg.Name).Int("dropped", dropped).Msg("hooks dropped due to missing scope")
		}
	}

	m.registry.SetState(reg.Name, StateRegistered)

	grantedStrings := permissions.ScopeStrings(grantedScopes)
	if err := conn.Send(gcpcv1.NewRegisterAck(true, "", grantedStrings)); err != nil {
		logger.Error().Str("plugin", reg.Name).Err(err).Msg("failed to send register ack")
		m.router.UnregisterPlugin(reg.Name)
		m.scopeRegistry.Unregister(reg.Name)
		_ = conn.Close()
		return
	}

	m.registry.SetState(reg.Name, StateRunning)
	logger.Info().Str("plugin", reg.Name).Str("version", reg.Version).Bool("critical", inst.Critical).Int("commands", len(reg.Commands)).Strs("scopes", grantedStrings).Msg("plugin registered")

	if m.eventBus != nil {
		m.eventBus.Emit(events.NewPluginRegistered(reg.Name, reg.Version, inst.Critical))
	}

	// Start health-check loop.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.healthLoop(inst)
	}()

	// Read loop for this plugin (handles ShutdownAck and future messages).
	m.readLoop(inst)
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
		logger.Warn().Str("plugin", pluginName).Strs("denied", permissions.ScopeStrings(denied)).
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
func (m *Manager) healthLoop(inst *PluginInstance) {
	ticker := time.NewTicker(m.cfg.HealthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if inst.State != StateRunning {
				return
			}
			if err := inst.Conn.Send(gcpcv1.NewHealthCheck()); err != nil {
				logger.Warn().Str("plugin", inst.Name).Err(err).Msg("health check send failed")
				m.registry.SetState(inst.Name, StateUnhealthy)
				return
			}
		}
	}
}

// readLoop reads messages from a connected plugin.
func (m *Manager) readLoop(inst *PluginInstance) {
	for {
		env, err := inst.Conn.Recv()
		if err != nil {
			if m.ctx.Err() != nil {
				return // shutting down
			}
			if inst.State == StateRunning {
				logger.Warn().Str("plugin", inst.Name).Err(err).Msg("plugin connection lost")
				m.router.UnregisterPlugin(inst.Name)
				m.hookRegistry.Unregister(inst.Name)
				m.scopeRegistry.Unregister(inst.Name)
				if m.eventBus != nil {
					m.eventBus.Unsubscribe("plugin:" + inst.Name)
				}
				m.registry.SetState(inst.Name, StateUnhealthy)
				if m.eventBus != nil {
					m.eventBus.Emit(events.NewPluginCrashed(inst.Name, inst.Critical, err.Error()))
				}
			}
			return
		}

		switch env.Payload.(type) {
		case *gcpcv1.EnvelopeV1_HealthResponse:
			resp := env.GetHealthResponse()
			if resp.Ok {
				inst.LastHealth = time.Now()
			} else {
				logger.Warn().Str("plugin", inst.Name).Str("status", resp.Status).Msg("plugin reported unhealthy")
				m.registry.SetState(inst.Name, StateUnhealthy)
				return
			}
		case *gcpcv1.EnvelopeV1_ShutdownAck:
			logger.Info().Str("plugin", inst.Name).Msg("plugin acknowledged shutdown")
			m.registry.SetState(inst.Name, StateShutdown)
			return
		case *gcpcv1.EnvelopeV1_EventSubscribe:
			sub := env.GetEventSubscribe()
			if !m.scopeRegistry.HasScope(inst.Name, permissions.ScopeEvents) {
				logger.Warn().Str("plugin", inst.Name).Msg("event subscription denied: missing 'events' scope")
				continue
			}
			if m.eventBus == nil {
				logger.Warn().Str("plugin", inst.Name).Msg("event subscription failed: event bus not set")
				continue
			}
			types := make([]events.Type, len(sub.Types))
			for i, t := range sub.Types {
				types[i] = events.Type(t)
			}
			// Bridge: subscribe on the server bus with a handler that forwards via GCPC.
			pluginConn := inst.Conn
			m.eventBus.Subscribe("plugin:"+inst.Name, types, func(evt events.Event) {
				gcpcEnv := &gcpcv1.EnvelopeV1{
					Version: gcpcv1.ProtocolVersion,
					Payload: &gcpcv1.EnvelopeV1_Event{Event: proto.Clone(evt.Proto).(*gcpcv1.EventV1)},
				}
				_ = pluginConn.Send(gcpcEnv)
			})
		case *gcpcv1.EnvelopeV1_ServerQuery:
			query := env.GetServerQuery()
			requiredScope := permissions.ScopeForTopic(query.Topic)
			if !m.scopeRegistry.HasScope(inst.Name, requiredScope) {
				_ = inst.Conn.Send(gcpcv1.NewServerQueryResponse(query.RequestId, nil,
					fmt.Sprintf("permission denied: missing scope %q", requiredScope)))
				continue
			}
			data, qErr := m.queryRegistry.Handle(query.Topic)
			errMsg := ""
			if qErr != nil {
				errMsg = qErr.Error()
			}
			_ = inst.Conn.Send(gcpcv1.NewServerQueryResponse(query.RequestId, data, errMsg))
		default:
			logger.Debug().Str("plugin", inst.Name).Msg("unexpected message from plugin")
		}
	}
}

// handlePluginExit handles unexpected plugin process termination.
func (m *Manager) handlePluginExit(inst *PluginInstance) {
	// Unregister commands, hooks, scopes, and event subscriptions.
	m.router.UnregisterPlugin(inst.Name)
	m.hookRegistry.Unregister(inst.Name)
	m.scopeRegistry.Unregister(inst.Name)
	if m.eventBus != nil {
		m.eventBus.Unsubscribe("plugin:" + inst.Name)
	}
	if m.eventBus != nil {
		m.eventBus.Emit(events.NewPluginCrashed(inst.Name, inst.Critical, "process exited unexpectedly"))
	}

	if inst.Critical {
		logger.Fatal().Str("plugin", inst.Name).Msg("critical plugin crashed — shutting down server")
		return
	}

	if inst.Restarts >= inst.MaxRestarts {
		logger.Error().Str("plugin", inst.Name).Int("restarts", inst.Restarts).Msg("max restarts exceeded, giving up")
		m.registry.SetState(inst.Name, StateShutdown)
		return
	}

	inst.Restarts++
	logger.Info().Str("plugin", inst.Name).Int("attempt", inst.Restarts).Msg("restarting non-critical plugin")
	m.registry.SetState(inst.Name, StateRestarting)

	if inst.Conn != nil {
		_ = inst.Conn.Close()
		inst.Conn = nil
	}

	m.launchPlugin(inst)
}
