package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	gcpc "gocache/api/gcpc/v1"
	apilogger "gocache/api/logger"
	apiplugin "gocache/api/plugin"
	"gocache/api/transport"
)

// Plugin is the interface plugin authors implement for lifecycle-only plugins
// (no command handling — health checks and shutdown only).
type Plugin interface {
	// Name returns the plugin's unique name (must match the binary filename).
	Name() string
	// Version returns the plugin version (semver recommended).
	Version() string
	// Critical returns whether the server should crash if this plugin fails.
	Critical() bool
	// OnHealthCheck is called when the server pings. Return nil for healthy.
	OnHealthCheck(ctx context.Context) error
	// OnShutdown is called when the server requests graceful shutdown.
	// The context carries the deadline.
	OnShutdown(ctx context.Context) error
}

// CommandPlugin extends Plugin with command registration and handling.
// Plugins that provide commands (either main namespace or REX namespaced)
// should implement this interface instead of Plugin.
type CommandPlugin interface {
	Plugin
	// Commands returns the list of commands this plugin provides.
	// Called once during registration.
	Commands() []CommandDecl
	// HandleCommand is called when a client invokes a plugin command.
	// metadata carries REX metadata with bare keys (no shared.rex. prefix), nil when absent.
	// Called concurrently from multiple goroutines — must be goroutine-safe.
	HandleCommand(ctx context.Context, cmd string, args []string, metadata map[string]string) *CommandResult
}

// HookPlugin extends Plugin with hook registration and handling.
// Plugins that want to intercept core commands implement this interface.
// A plugin can implement both CommandPlugin and HookPlugin.
type HookPlugin interface {
	Plugin
	// Hooks returns the hook declarations for this plugin.
	// Called once during registration.
	Hooks() []HookDecl
	// HandleHook is called when a matching hook fires.
	// For critical pre-hooks, returning Deny=true aborts the command.
	// Called concurrently from multiple goroutines — must be goroutine-safe.
	HandleHook(ctx context.Context, req *HookRequest) *HookResponse
}

// ScopePlugin is an optional interface for plugins that declare required scopes.
// Plugins that do not implement this interface receive the default scope ["read"].
type ScopePlugin interface {
	Plugin
	// Scopes returns the scopes this plugin requests (e.g. "read", "write", "hook:pre", "keys:prefix:*").
	// Called once during registration.
	Scopes() []string
}

// QueryPlugin is an optional interface for plugins that need to query server state.
// SetSession is called once after registration, before the message loop starts.
type QueryPlugin interface {
	Plugin
	// SetSession provides access to the Session for querying the server.
	SetSession(s *Session)
}

// EventPlugin is an optional interface for plugins that subscribe to server events.
type EventPlugin interface {
	Plugin
	// EventTypes returns the event types this plugin subscribes to.
	EventTypes() []string
	// HandleEvent is called when a subscribed event fires.
	// The EventV1 proto carries strongly-typed data in a oneof field.
	// Called concurrently — must be goroutine-safe.
	HandleEvent(ctx context.Context, evt *gcpc.EventV1)
}

// OperationHookPlugin extends Plugin with operation lifecycle hooks.
// Plugins implementing this interface are called synchronously when operations
// start (to enrich context) and asynchronously when operations complete.
type OperationHookPlugin interface {
	Plugin
	// OperationHooks returns the operation types this plugin hooks into.
	OperationHooks() []OperationHookDecl
	// HandleOperationHook is called when a matching operation starts or completes.
	// For start phase: response ContextValues are merged into the operation context.
	// For complete phase: response is ignored (fire-and-forget).
	HandleOperationHook(ctx context.Context, req *OperationHookRequest) *OperationHookResponse
}

// OperationHookDecl declares an operation hook.
type OperationHookDecl struct {
	Type     string // operation type to match, "*" for all
	Priority int    // lower = fires first
}

// OperationHookRequest carries operation data to the plugin handler.
type OperationHookRequest struct {
	OperationID   string
	OperationType string
	ParentID      string
	Phase         string            // apiplugin.PhaseStart or apiplugin.PhaseComplete
	Context       map[string]string // filtered for this plugin's visibility
}

// OperationHookResponse is the plugin's response to an operation start hook.
type OperationHookResponse struct {
	ContextValues map[string]string // values to merge into operation context
}

// HookPhase indicates when a hook fires relative to command execution.
type HookPhase int

const (
	HookPhasePre  HookPhase = 1
	HookPhasePost HookPhase = 2
)

// CommandDecl declares a command a plugin can handle.
type CommandDecl struct {
	Name       string // command name (e.g. "PUBLISH" or "QUERY")
	Namespaced bool   // true = REX (PLUGIN:CMD), false = main namespace
	MinArgs    int
	MaxArgs    int  // -1 = unlimited
	ReadOnly   bool // hint: command does not mutate state
}

// CommandResult holds the result of a plugin command execution.
type CommandResult struct {
	// Value can be: string, int, int64, float64, nil, error,
	// []any, []string, map[string]string, map[string]any.
	Value any
}

// HookDecl declares a hook a plugin wants to intercept.
type HookDecl struct {
	Pattern string    // "SET", "GET", "*" (exact or wildcard)
	Phase   HookPhase // Pre or Post
}

// HookRequest contains the context for a hook invocation.
type HookRequest struct {
	Phase       HookPhase
	Command     string
	Args        []string
	ResultValue string            // post-hook only
	ResultError string            // post-hook only
	Context     map[string]string // accumulated context from server + own namespace + shared
	Metadata    map[string]string // REX metadata with bare keys (no shared.rex. prefix)
}

// HookResponse is the plugin's response to a hook invocation.
type HookResponse struct {
	Deny          bool // pre-hook only: true = abort the command
	DenyReason    string
	ContextValues map[string]string // pre-hook: values to add to command context
}

// Run connects to the GoCache server's plugin socket, registers the plugin,
// and enters the message loop. It blocks until shutdown or context cancellation.
// If the plugin implements CommandPlugin, its commands are registered.
// If the plugin implements HookPlugin, its hooks are registered.
func Run(ctx context.Context, p Plugin) error {
	// Create a logger for this plugin, writing to stdout.
	pluginLog := apilogger.New(os.Stdout, p.Name(), "debug")

	sockPath := os.Getenv(apiplugin.EnvSocketPath)
	if sockPath == "" {
		return fmt.Errorf("%s not set", apiplugin.EnvSocketPath)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial plugin socket: %w", err)
	}
	tc := transport.NewConn(conn)
	defer tc.Close()

	// Track handler goroutines so Run waits for them before returning —
	// prevents a concurrent OnShutdown from tearing down state while a
	// handler is still using it.
	var handlerWg sync.WaitGroup
	defer handlerWg.Wait()

	// Detect interface support.
	cp, isCommandPlugin := p.(CommandPlugin)
	hp, isHookPlugin := p.(HookPlugin)
	sp, isScopePlugin := p.(ScopePlugin)
	qp, isQueryPlugin := p.(QueryPlugin)
	ep, isEventPlugin := p.(EventPlugin)
	ohp, isOperationHookPlugin := p.(OperationHookPlugin)

	// Build registration message.
	var cmdDecls []*gcpc.CommandDeclV1
	if isCommandPlugin {
		decls := cp.Commands()
		cmdDecls = make([]*gcpc.CommandDeclV1, len(decls))
		for i, d := range decls {
			cmdDecls[i] = &gcpc.CommandDeclV1{
				Name:       d.Name,
				Namespaced: d.Namespaced,
				MinArgs:    int32(d.MinArgs),
				MaxArgs:    int32(d.MaxArgs),
				Readonly:   d.ReadOnly,
			}
		}
	}

	var hookDecls []*gcpc.HookDeclV1
	if isHookPlugin {
		decls := hp.Hooks()
		hookDecls = make([]*gcpc.HookDeclV1, len(decls))
		for i, d := range decls {
			hookDecls[i] = &gcpc.HookDeclV1{
				Pattern: d.Pattern,
				Phase:   gcpc.HookPhaseV1(d.Phase),
			}
		}
	}

	// Collect requested scopes.
	var requestedScopes []string
	if isScopePlugin {
		requestedScopes = sp.Scopes()
	}

	// Build operation hook declarations.
	var opHookDecls []*gcpc.OperationHookDeclV1
	if isOperationHookPlugin {
		decls := ohp.OperationHooks()
		opHookDecls = make([]*gcpc.OperationHookDeclV1, len(decls))
		for i, d := range decls {
			opHookDecls[i] = &gcpc.OperationHookDeclV1{
				Type:     d.Type,
				Priority: int32(d.Priority),
			}
		}
	}

	// Send registration.
	reg := &gcpc.RegisterV1{
		Name:            p.Name(),
		Version:         p.Version(),
		Critical:        p.Critical(),
		Commands:        cmdDecls,
		Hooks:           hookDecls,
		RequestedScopes: requestedScopes,
		OperationHooks:  opHookDecls,
	}
	env := &gcpc.EnvelopeV1{
		Version: gcpc.ProtocolVersion,
		Payload: &gcpc.EnvelopeV1_Register{Register: reg},
	}
	if err := tc.Send(env); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	// Receive ack.
	ackEnv, err := tc.Recv()
	if err != nil {
		return fmt.Errorf("recv register ack: %w", err)
	}
	ack := ackEnv.GetRegisterAck()
	if ack == nil {
		return errors.New("expected RegisterAck, got different message")
	}
	if !ack.Accepted {
		return fmt.Errorf("registration rejected: %s", ack.Reason)
	}
	if len(ack.GrantedScopes) > 0 {
		pluginLog.InfoNoCtx().Strs("scopes", ack.GrantedScopes).Msg("granted scopes")
	}
	// Log denied scopes so the plugin author knows which features will be unavailable.
	if isScopePlugin {
		grantedSet := make(map[string]struct{}, len(ack.GrantedScopes))
		for _, s := range ack.GrantedScopes {
			grantedSet[s] = struct{}{}
		}
		var denied []string
		for _, s := range sp.Scopes() {
			if _, ok := grantedSet[s]; !ok {
				denied = append(denied, s)
			}
		}
		if len(denied) > 0 {
			pluginLog.WarnNoCtx().Strs("denied", denied).Msg("scopes denied — features requiring these scopes will return errors")
		}
	}

	// Set up session for server queries.
	session := newSession(tc)
	if isQueryPlugin {
		qp.SetSession(session)
	}

	// Subscribe to events if the plugin implements EventPlugin.
	if isEventPlugin {
		if err := tc.Send(gcpc.NewEventSubscribe(ep.EventTypes())); err != nil {
			return fmt.Errorf("send event subscribe: %w", err)
		}
	}

	// Enter message loop.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		env, err := tc.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("recv: %w", err)
		}

		switch env.Payload.(type) {
		case *gcpc.EnvelopeV1_HealthCheck:
			hErr := p.OnHealthCheck(ctx)
			ok := hErr == nil
			status := ""
			if hErr != nil {
				status = hErr.Error()
			}
			if err := tc.Send(gcpc.NewHealthResponse(ok, status)); err != nil {
				return fmt.Errorf("send health response: %w", err)
			}

		case *gcpc.EnvelopeV1_Shutdown:
			sd := env.GetShutdown()
			deadline := time.Unix(0, int64(sd.DeadlineNs))
			sdCtx, cancel := context.WithDeadline(ctx, deadline)
			_ = p.OnShutdown(sdCtx)
			cancel()

			if err := tc.Send(gcpc.NewShutdownAck()); err != nil {
				return fmt.Errorf("send shutdown ack: %w", err)
			}
			return nil

		case *gcpc.EnvelopeV1_CommandRequest:
			if !isCommandPlugin {
				continue
			}
			req := env.GetCommandRequest()
			handlerWg.Add(1)
			go func() {
				defer handlerWg.Done()
				result := cp.HandleCommand(ctx, req.Command, req.Args, req.Metadata)
				var protoResult *gcpc.ResultV1
				if result != nil {
					protoResult = gcpc.ResultFromInterface(result.Value)
				} else {
					protoResult = gcpc.ResultFromInterface(nil)
				}
				resp := gcpc.NewCommandResponse(req.RequestId, protoResult)
				if err := tc.Send(resp); err != nil {
					pluginLog.ErrorNoCtx().Err(err).Str("command", req.Command).Msg("failed to send command response")
				}
			}()

		case *gcpc.EnvelopeV1_OperationHookRequest:
			if !isOperationHookPlugin {
				continue
			}
			req := env.GetOperationHookRequest()
			hookReq := &OperationHookRequest{
				OperationID:   req.OperationId,
				OperationType: req.OperationType,
				ParentID:      req.ParentId,
				Phase:         req.Phase,
				Context:       req.Context,
			}
			if req.Phase == apiplugin.PhaseStart {
				// Start phase: synchronous — server is waiting for response.
				result := ohp.HandleOperationHook(ctx, hookReq)
				var ctxValues map[string]string
				if result != nil {
					ctxValues = result.ContextValues
				}
				resp := gcpc.NewOperationHookResponse(req.RequestId, ctxValues)
				if err := tc.Send(resp); err != nil {
					pluginLog.ErrorNoCtx().Err(err).Msg("failed to send operation hook response")
				}
			} else {
				// Complete phase: fire-and-forget.
				handlerWg.Add(1)
				go func() {
					defer handlerWg.Done()
					ohp.HandleOperationHook(ctx, hookReq)
				}()
			}

		case *gcpc.EnvelopeV1_Event:
			if isEventPlugin {
				evt := env.GetEvent()
				handlerWg.Add(1)
				go func() {
					defer handlerWg.Done()
					ep.HandleEvent(ctx, evt)
				}()
			}

		case *gcpc.EnvelopeV1_ServerQueryResponse:
			resp := env.GetServerQueryResponse()
			session.dispatch(resp)

		case *gcpc.EnvelopeV1_HookRequest:
			if !isHookPlugin {
				continue
			}
			req := env.GetHookRequest()
			handlerWg.Add(1)
			go func() {
				defer handlerWg.Done()
				hookReq := &HookRequest{
					Phase:       HookPhase(req.Phase),
					Command:     req.Command,
					Args:        req.Args,
					ResultValue: req.ResultValue,
					ResultError: req.ResultError,
					Context:     req.Context,
					Metadata:    req.Metadata,
				}
				result := hp.HandleHook(ctx, hookReq)
				deny := false
				denyReason := ""
				var ctxValues map[string]string
				if result != nil {
					deny = result.Deny
					denyReason = result.DenyReason
					ctxValues = result.ContextValues
				}
				resp := gcpc.NewHookResponse(req.RequestId, deny, denyReason, ctxValues)
				if err := tc.Send(resp); err != nil {
					pluginLog.ErrorNoCtx().Err(err).Str("command", req.Command).Msg("failed to send hook response")
				}
			}()
		}
	}
}
