package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	gcpc "gocache/api/gcpc/v1"
	"gocache/api/logger"
	"gocache/api/transport"
)

var (
	ErrCommandNotFound = errors.New("unknown command")
	ErrPluginDown      = errors.New("plugin connection unavailable")
	ErrPluginTimeout   = errors.New("plugin command timed out")
	ErrShadowCore      = errors.New("cannot shadow core command")
	ErrDuplicateCmd    = errors.New("command already registered by another plugin")
)

// requestSeq is a package-private monotonic counter; callers outside the
// package use NextRequestID to mint IDs.
var requestSeq atomic.Uint64

// NextRequestID returns a new unique request identifier for plugin calls.
func NextRequestID() string {
	return fmt.Sprintf("req-%d", requestSeq.Add(1))
}

// PluginRoute describes a single command route to a plugin.
type PluginRoute struct {
	PluginName string
	Command    string // normalized UPPER, the command name without namespace prefix
	FullKey    string // the key in the commands map (e.g. "PUBLISH" or "KAFKA:PUBLISH")
	Namespaced bool
	MinArgs    int
	MaxArgs    int
	ReadOnly   bool
}

// PluginConn wraps a transport.Conn with request/response multiplexing.
// Multiple goroutines can send requests concurrently; responses are
// correlated by request_id. Used by both the command router and hook executor.
type PluginConn struct {
	mu        sync.Mutex // serializes writes
	conn      *transport.Conn
	pending   sync.Map // map[requestID]chan *gcpc.EnvelopeV1
	done      chan struct{}
	closeOnce sync.Once
	Name      string // plugin name for logging
}

func NewPluginConn(name string, conn *transport.Conn) *PluginConn {
	pc := &PluginConn{
		conn: conn,
		done: make(chan struct{}),
		Name: name,
	}
	go pc.readLoop()
	return pc
}

// Send writes an envelope and returns a channel for the correlated response.
// The write is done asynchronously so we can respect context cancellation
// even if the plugin is slow to read.
func (pc *PluginConn) Send(ctx context.Context, req *gcpc.EnvelopeV1, requestID string) (<-chan *gcpc.EnvelopeV1, error) {
	ch := make(chan *gcpc.EnvelopeV1, 1)
	pc.pending.Store(requestID, ch)

	// Async write to avoid blocking if plugin is slow to read.
	errCh := make(chan error, 1)
	go func() {
		pc.mu.Lock()
		err := pc.conn.Send(req)
		pc.mu.Unlock()
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			pc.pending.Delete(requestID)
			return nil, err
		}
		return ch, nil
	case <-ctx.Done():
		pc.pending.Delete(requestID)
		return nil, ctx.Err()
	case <-pc.done:
		pc.pending.Delete(requestID)
		return nil, ErrPluginDown
	}
}

// SendFireAndForget writes an envelope without waiting for a response.
func (pc *PluginConn) SendFireAndForget(env *gcpc.EnvelopeV1) {
	pc.mu.Lock()
	_ = pc.conn.Send(env)
	pc.mu.Unlock()
}

// readLoop reads all incoming envelopes and dispatches responses
// to the appropriate pending channel by request_id. Handles every
// request/response type that flows through PluginConn: command,
// command hook, and operation hook.
func (pc *PluginConn) readLoop() {
	defer pc.drainPending()
	for {
		select {
		case <-pc.done:
			return
		default:
		}

		env, err := pc.conn.Recv()
		if err != nil {
			select {
			case <-pc.done:
			default:
				logger.DebugNoCtx().Str("plugin", pc.Name).Err(err).Msg("plugin conn read error")
			}
			return
		}

		// Extract request_id from known response types.
		var reqID string
		switch {
		case env.GetCommandResponse() != nil:
			reqID = env.GetCommandResponse().RequestId
		case env.GetHookResponse() != nil:
			reqID = env.GetHookResponse().RequestId
		case env.GetOperationHookResponse() != nil:
			reqID = env.GetOperationHookResponse().RequestId
		default:
			continue // not a response type we handle
		}

		if ch, ok := pc.pending.LoadAndDelete(reqID); ok {
			ch.(chan *gcpc.EnvelopeV1) <- env
		}
	}
}

// drainPending closes all pending channels so waiters unblock.
func (pc *PluginConn) drainPending() {
	pc.pending.Range(func(key, value any) bool {
		close(value.(chan *gcpc.EnvelopeV1))
		pc.pending.Delete(key)
		return true
	})
}

// Close signals the readLoop to stop and closes the underlying transport.
// Closing the transport is required to unblock a readLoop that is parked
// in io.ReadFull. Safe to call multiple times.
func (pc *PluginConn) Close() {
	pc.closeOnce.Do(func() {
		close(pc.done)
		if pc.conn != nil {
			_ = pc.conn.Close()
		}
	})
}

// Done returns a channel that is closed when the connection is shut down.
func (pc *PluginConn) Done() <-chan struct{} {
	return pc.done
}

// DeletePending removes a pending request (used for cleanup on timeout).
func (pc *PluginConn) DeletePending(requestID string) {
	pc.pending.Delete(requestID)
}

// Router maps command names to plugin connections and handles IPC dispatch.
type Router struct {
	mu           sync.RWMutex
	routes       map[string]*PluginRoute // full key (upper) → route
	conns        map[string]*PluginConn  // full key (upper) → plugin conn
	plugins      map[string]*PluginConn  // plugin name → conn wrapper
	pluginRoutes map[string][]string     // plugin name → list of full keys (for cleanup)
	coreCommands map[string]struct{}
}

// NewRouter creates a command router. coreCommands is the list of command
// names handled by the core evaluator (used to reject shadowing).
func NewRouter(coreCommands []string) *Router {
	core := make(map[string]struct{}, len(coreCommands))
	for _, cmd := range coreCommands {
		core[strings.ToUpper(cmd)] = struct{}{}
	}
	return &Router{
		routes:       make(map[string]*PluginRoute),
		conns:        make(map[string]*PluginConn),
		plugins:      make(map[string]*PluginConn),
		pluginRoutes: make(map[string][]string),
		coreCommands: core,
	}
}

// GetPluginConn returns the connection for a plugin, or nil if not found.
func (r *Router) GetPluginConn(name string) *PluginConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.plugins[name]
}

// RegisterPlugin registers all commands declared by a plugin.
// Returns an error if any command shadows a core command or is already
// registered by another plugin. On error, no commands are registered
// (atomic: all or nothing).
func (r *Router) RegisterPlugin(name string, conn *transport.Conn, decls []*gcpc.CommandDeclV1) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Compute all keys first and validate before mutating state.
	type pending struct {
		key   string
		route *PluginRoute
	}
	var toAdd []pending

	for _, d := range decls {
		cmdName := strings.ToUpper(d.Name)
		var fullKey string
		if d.Namespaced {
			fullKey = strings.ToUpper(name) + ":" + cmdName
		} else {
			fullKey = cmdName
		}

		// Reject shadowing core commands.
		if _, isCore := r.coreCommands[fullKey]; isCore {
			return fmt.Errorf("%w: %s", ErrShadowCore, fullKey)
		}

		// Reject duplicate registration by another plugin.
		if existing, exists := r.routes[fullKey]; exists && existing.PluginName != name {
			return fmt.Errorf("%w: %s (owned by %s)", ErrDuplicateCmd, fullKey, existing.PluginName)
		}

		toAdd = append(toAdd, pending{
			key: fullKey,
			route: &PluginRoute{
				PluginName: name,
				Command:    cmdName,
				FullKey:    fullKey,
				Namespaced: d.Namespaced,
				MinArgs:    int(d.MinArgs),
				MaxArgs:    int(d.MaxArgs),
				ReadOnly:   d.Readonly,
			},
		})
	}

	// All validated — apply.
	pc := NewPluginConn(name, conn)
	r.plugins[name] = pc

	keys := make([]string, 0, len(toAdd))
	for _, p := range toAdd {
		r.routes[p.key] = p.route
		r.conns[p.key] = pc
		keys = append(keys, p.key)
	}
	r.pluginRoutes[name] = keys

	logger.InfoNoCtx().Str("plugin", name).Int("commands", len(toAdd)).Msg("plugin commands registered")
	return nil
}

// UnregisterPlugin removes all routes owned by the named plugin and
// closes its multiplexed connection.
func (r *Router) UnregisterPlugin(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	keys := r.pluginRoutes[name]
	for _, k := range keys {
		delete(r.routes, k)
		delete(r.conns, k)
	}
	delete(r.pluginRoutes, name)

	if pc, ok := r.plugins[name]; ok {
		pc.Close()
		delete(r.plugins, name)
	}

	if len(keys) > 0 {
		logger.InfoNoCtx().Str("plugin", name).Int("commands", len(keys)).Msg("plugin commands unregistered")
	}
}

// HasCommand returns true if op (or REX-parsed op) maps to a plugin command.
func (r *Router) HasCommand(op string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, _, found := r.lookup(op)
	return found
}

// Route dispatches a command to the owning plugin and waits for the response.
// Returns the result as an any compatible with evaluator.Result.Value.
// metadata carries REX metadata with bare keys (no shared.rex. prefix).
func (r *Router) Route(ctx context.Context, op string, args []string, metadata map[string]string) (any, error) {
	r.mu.RLock()
	route, pc, found := r.lookup(op)
	r.mu.RUnlock()

	if !found {
		return nil, ErrCommandNotFound
	}

	// Arg count validation.
	n := len(args)
	if n < route.MinArgs || (route.MaxArgs >= 0 && n > route.MaxArgs) {
		return nil, fmt.Errorf("ERR wrong number of arguments for '%s' command", strings.ToLower(op))
	}

	requestID := NextRequestID()
	env := gcpc.NewCommandRequest(route.Command, args, requestID, metadata)

	respCh, err := pc.Send(ctx, env, requestID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrPluginTimeout
		}
		return nil, fmt.Errorf("%w: %s", ErrPluginDown, err.Error())
	}

	select {
	case resp, ok := <-respCh:
		if !ok {
			return nil, ErrPluginDown
		}
		cmdResp := resp.GetCommandResponse()
		if cmdResp == nil {
			return nil, ErrPluginDown
		}
		return gcpc.InterfaceFromResult(cmdResp.Result), nil
	case <-ctx.Done():
		pc.pending.Delete(requestID)
		return nil, ErrPluginTimeout
	}
}

// lookup finds a route and its connection. Must be called with r.mu held (read).
// The routes map is keyed by the full upper-case command name — REX-namespaced
// entries are stored as "PLUGIN:CMD" at registration time, so a single
// case-insensitive lookup covers both main-namespace and REX commands.
func (r *Router) lookup(op string) (*PluginRoute, *PluginConn, bool) {
	up := strings.ToUpper(op)
	if route, ok := r.routes[up]; ok {
		return route, r.conns[up], true
	}
	return nil, nil, false
}
