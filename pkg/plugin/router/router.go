package router

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gcpc "gocache/api/gcpc/v1"
	ops "gocache/api/operations"
	"gocache/commons/logger"
	"gocache/commons/transport"
)

var (
	ErrCommandNotFound = errors.New("unknown command")
	ErrPluginDown      = errors.New("plugin connection unavailable")
	ErrPluginTimeout   = errors.New("plugin command timed out")
	ErrPluginQueueFull = errors.New("plugin outbound queue full")
	ErrShadowCore      = errors.New("cannot shadow core command")
	ErrDuplicateCmd    = errors.New("command already registered by another plugin")

	errNilOutboundEnvelope = errors.New("nil outbound envelope")
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

// RouteMeta is a lightweight snapshot of a registered command's metadata,
// returned by LookupMeta for callers that need classification without routing.
type RouteMeta struct {
	ReadOnly bool
}

const (
	pluginOutboundQueueSize     = 1024
	pluginOutboundBatchMax      = 32
	pluginOutboundBatchMaxDelay = 200 * time.Microsecond
)

// PluginConn wraps a transport.Conn with request/response multiplexing.
// Multiple goroutines can send requests concurrently; responses are
// correlated by request_id. Used by both the command router and hook executor.
type PluginConn struct {
	conn      *transport.Conn
	pending   sync.Map // map[requestID]chan *gcpc.EnvelopeV1
	pendingMu sync.Mutex

	sendMu     sync.Mutex
	closed     bool
	outbound   chan outboundEnvelope
	writerDone chan struct{}

	done      chan struct{}
	closeOnce sync.Once
	Name      string // plugin name for logging
	stats     pluginConnStats
}

type pluginConnStats struct {
	sendAttempts               atomic.Uint64
	sendAccepted               atomic.Uint64
	sendQueueFull              atomic.Uint64
	sendPluginDown             atomic.Uint64
	sendContextCancelled       atomic.Uint64
	blockingSendAttempts       atomic.Uint64
	blockingSendLatencyTotalNs atomic.Uint64
	blockingSendLatencyMaxNs   atomic.Uint64
	fireAndForgetAttempts      atomic.Uint64
	fireAndForgetAccepted      atomic.Uint64
	fireAndForgetDrops         atomic.Uint64
	enqueueLatencyTotalNs      atomic.Uint64
	enqueueLatencyMaxNs        atomic.Uint64
	writeAttempts              atomic.Uint64
	writeErrors                atomic.Uint64
	writeBatches               atomic.Uint64
	writeBatchEnvelopes        atomic.Uint64
	writeBatchMaxSize          atomic.Uint64
	writeLatencyTotalNs        atomic.Uint64
	writeLatencyMaxNs          atomic.Uint64
	queueLagTotalNs            atomic.Uint64
	queueLagMaxNs              atomic.Uint64
}

// PluginConnStats is a point-in-time snapshot of one plugin connection's
// outbound IPC queue and writer measurements. Counters are monotonic since the
// connection was created; queue depth is sampled at snapshot time.
type PluginConnStats struct {
	PluginName                 string
	QueueCapacity              int
	QueueDepth                 int
	SendAttempts               uint64
	SendAccepted               uint64
	SendQueueFull              uint64
	SendPluginDown             uint64
	SendContextCancelled       uint64
	BlockingSendAttempts       uint64
	BlockingSendLatencyTotalNs uint64
	BlockingSendLatencyMaxNs   uint64
	FireAndForgetAttempts      uint64
	FireAndForgetAccepted      uint64
	FireAndForgetDrops         uint64
	EnqueueLatencyTotalNs      uint64
	EnqueueLatencyMaxNs        uint64
	WriteAttempts              uint64
	WriteErrors                uint64
	WriteBatches               uint64
	WriteBatchEnvelopes        uint64
	WriteBatchMaxSize          uint64
	WriteLatencyTotalNs        uint64
	WriteLatencyMaxNs          uint64
	QueueLagTotalNs            uint64
	QueueLagMaxNs              uint64
}

type outboundEnvelope struct {
	env        *gcpc.EnvelopeV1
	build      func() *gcpc.EnvelopeV1
	errCh      chan error
	enqueuedAt time.Time
}

func (item outboundEnvelope) envelope() *gcpc.EnvelopeV1 {
	if item.env != nil {
		return item.env
	}
	if item.build != nil {
		return item.build()
	}
	return nil
}

func NewPluginConn(name string, conn *transport.Conn) *PluginConn {
	pc := &PluginConn{
		conn:       conn,
		outbound:   make(chan outboundEnvelope, pluginOutboundQueueSize),
		writerDone: make(chan struct{}),
		done:       make(chan struct{}),
		Name:       name,
	}
	go pc.writeLoop()
	return pc
}

func (pc *PluginConn) writeLoop() {
	defer close(pc.writerDone)
	for {
		select {
		case item := <-pc.outbound:
			pc.writeOutboundBatch(pc.collectBatch(item))
		case <-pc.done:
			for {
				select {
				case item := <-pc.outbound:
					pc.writeOutboundBatch(pc.collectBatch(item))
				default:
					return
				}
			}
		}
	}
}

func (pc *PluginConn) collectBatch(first outboundEnvelope) []outboundEnvelope {
	return pc.collectBatchWithDelay(first, pluginOutboundBatchMaxDelay)
}

func (pc *PluginConn) collectBatchWithDelay(first outboundEnvelope, maxDelay time.Duration) []outboundEnvelope {
	batch := make([]outboundEnvelope, 0, pluginOutboundBatchMax)
	batch = append(batch, first)

	hasBlocking := first.errCh != nil
	batch, drainedBlocking := pc.drainAvailable(batch)
	hasBlocking = hasBlocking || drainedBlocking
	if hasBlocking || len(batch) == pluginOutboundBatchMax || pc.doneClosed() {
		return batch
	}

	timer := time.NewTimer(maxDelay)
	defer timer.Stop()
	for len(batch) < pluginOutboundBatchMax {
		select {
		case item := <-pc.outbound:
			batch = append(batch, item)
			if item.errCh != nil {
				batch, _ = pc.drainAvailable(batch)
				return batch
			}
		case <-timer.C:
			batch, _ = pc.drainAvailable(batch)
			return batch
		case <-pc.done:
			batch, _ = pc.drainAvailable(batch)
			return batch
		}
	}
	return batch
}

func (pc *PluginConn) drainAvailable(batch []outboundEnvelope) ([]outboundEnvelope, bool) {
	hasBlocking := false
	for len(batch) < pluginOutboundBatchMax {
		select {
		case item := <-pc.outbound:
			if item.errCh != nil {
				hasBlocking = true
			}
			batch = append(batch, item)
		default:
			return batch, hasBlocking
		}
	}
	return batch, hasBlocking
}

func (pc *PluginConn) doneClosed() bool {
	select {
	case <-pc.done:
		return true
	default:
		return false
	}
}

func (pc *PluginConn) writeOutboundBatch(batch []outboundEnvelope) {
	if len(batch) == 0 {
		return
	}

	envs := make([]*gcpc.EnvelopeV1, 0, len(batch))
	nilEnvelopes := 0
	for i := range batch {
		item := &batch[i]
		if !item.enqueuedAt.IsZero() {
			observeDuration(&pc.stats.queueLagTotalNs, &pc.stats.queueLagMaxNs, time.Since(item.enqueuedAt))
		}
		env := item.envelope()
		if env == nil {
			nilEnvelopes++
			continue
		}
		item.env = env
		item.build = nil
		envs = append(envs, env)
	}

	pc.stats.writeAttempts.Add(uint64(len(batch)))
	pc.stats.writeBatches.Add(1)
	pc.stats.writeBatchEnvelopes.Add(uint64(len(envs)))
	observeMax(&pc.stats.writeBatchMaxSize, uint64(len(envs)))

	start := time.Now()
	var writeErr error
	if len(envs) == 1 {
		writeErr = pc.conn.Send(envs[0])
	} else if len(envs) > 1 {
		writeErr = pc.conn.SendBatch(envs)
	}
	observeDuration(&pc.stats.writeLatencyTotalNs, &pc.stats.writeLatencyMaxNs, time.Since(start))
	if writeErr != nil {
		pc.stats.writeErrors.Add(uint64(len(envs)))
	}
	if nilEnvelopes > 0 {
		pc.stats.writeErrors.Add(uint64(nilEnvelopes))
	}
	for _, item := range batch {
		if item.errCh != nil {
			if item.env == nil {
				item.errCh <- errNilOutboundEnvelope
				continue
			}
			item.errCh <- writeErr
		}
	}
}

func (pc *PluginConn) enqueue(ctx context.Context, item outboundEnvelope) error {
	pc.stats.sendAttempts.Add(1)
	if item.errCh == nil {
		pc.stats.fireAndForgetAttempts.Add(1)
	}
	start := time.Now()
	pc.sendMu.Lock()
	defer pc.sendMu.Unlock()
	defer func() {
		observeDuration(&pc.stats.enqueueLatencyTotalNs, &pc.stats.enqueueLatencyMaxNs, time.Since(start))
	}()

	if pc.closed {
		pc.stats.sendPluginDown.Add(1)
		if item.errCh == nil {
			pc.stats.fireAndForgetDrops.Add(1)
		}
		return ErrPluginDown
	}

	item.enqueuedAt = time.Now()
	select {
	case pc.outbound <- item:
		pc.stats.sendAccepted.Add(1)
		if item.errCh == nil {
			pc.stats.fireAndForgetAccepted.Add(1)
		}
		return nil
	case <-ctx.Done():
		pc.stats.sendContextCancelled.Add(1)
		if item.errCh == nil {
			pc.stats.fireAndForgetDrops.Add(1)
		}
		return ctx.Err()
	default:
		pc.stats.sendQueueFull.Add(1)
		if item.errCh == nil {
			pc.stats.fireAndForgetDrops.Add(1)
		}
		return ErrPluginQueueFull
	}
}

// Send enqueues an envelope for the per-plugin FIFO writer and returns a
// channel for the correlated response after the frame has been accepted by the
// transport. Context cancellation can still win if the plugin is slow to read.
func (pc *PluginConn) Send(ctx context.Context, req *gcpc.EnvelopeV1, requestID string) (<-chan *gcpc.EnvelopeV1, error) {
	pc.stats.blockingSendAttempts.Add(1)
	start := time.Now()
	defer func() {
		observeDuration(&pc.stats.blockingSendLatencyTotalNs, &pc.stats.blockingSendLatencyMaxNs, time.Since(start))
	}()

	ch := make(chan *gcpc.EnvelopeV1, 1)
	pc.storePending(requestID, ch)

	errCh := make(chan error, 1)
	if err := pc.enqueue(ctx, outboundEnvelope{env: req, errCh: errCh}); err != nil {
		pc.DeletePending(requestID)
		return nil, err
	}

	select {
	case err := <-errCh:
		if err != nil {
			pc.DeletePending(requestID)
			return nil, err
		}
		return ch, nil
	case <-ctx.Done():
		pc.DeletePending(requestID)
		return nil, ctx.Err()
	case <-pc.done:
		pc.DeletePending(requestID)
		return nil, ErrPluginDown
	}
}

// SendFireAndForget enqueues an envelope without waiting for a response. If the
// plugin is down or its outbound queue is full, the frame is dropped; callers
// have no response path for these best-effort messages.
func (pc *PluginConn) SendFireAndForget(env *gcpc.EnvelopeV1) {
	_ = pc.enqueue(context.Background(), outboundEnvelope{env: env})
}

// SendFireAndForgetLazy enqueues a best-effort envelope builder. The builder is
// evaluated by the FIFO writer only after the frame has been accepted into the
// outbound queue, so overloaded IPC plugins can drop observability frames
// without paying projection/protobuf-envelope construction costs first.
func (pc *PluginConn) SendFireAndForgetLazy(build func() *gcpc.EnvelopeV1) {
	if build == nil {
		return
	}
	_ = pc.enqueue(context.Background(), outboundEnvelope{build: build})
}

// Stats returns a point-in-time snapshot of outbound queue and write-loop
// measurements for this plugin connection.
func (pc *PluginConn) Stats() PluginConnStats {
	return PluginConnStats{
		PluginName:                 pc.Name,
		QueueCapacity:              cap(pc.outbound),
		QueueDepth:                 len(pc.outbound),
		SendAttempts:               pc.stats.sendAttempts.Load(),
		SendAccepted:               pc.stats.sendAccepted.Load(),
		SendQueueFull:              pc.stats.sendQueueFull.Load(),
		SendPluginDown:             pc.stats.sendPluginDown.Load(),
		SendContextCancelled:       pc.stats.sendContextCancelled.Load(),
		BlockingSendAttempts:       pc.stats.blockingSendAttempts.Load(),
		BlockingSendLatencyTotalNs: pc.stats.blockingSendLatencyTotalNs.Load(),
		BlockingSendLatencyMaxNs:   pc.stats.blockingSendLatencyMaxNs.Load(),
		FireAndForgetAttempts:      pc.stats.fireAndForgetAttempts.Load(),
		FireAndForgetAccepted:      pc.stats.fireAndForgetAccepted.Load(),
		FireAndForgetDrops:         pc.stats.fireAndForgetDrops.Load(),
		EnqueueLatencyTotalNs:      pc.stats.enqueueLatencyTotalNs.Load(),
		EnqueueLatencyMaxNs:        pc.stats.enqueueLatencyMaxNs.Load(),
		WriteAttempts:              pc.stats.writeAttempts.Load(),
		WriteErrors:                pc.stats.writeErrors.Load(),
		WriteBatches:               pc.stats.writeBatches.Load(),
		WriteBatchEnvelopes:        pc.stats.writeBatchEnvelopes.Load(),
		WriteBatchMaxSize:          pc.stats.writeBatchMaxSize.Load(),
		WriteLatencyTotalNs:        pc.stats.writeLatencyTotalNs.Load(),
		WriteLatencyMaxNs:          pc.stats.writeLatencyMaxNs.Load(),
		QueueLagTotalNs:            pc.stats.queueLagTotalNs.Load(),
		QueueLagMaxNs:              pc.stats.queueLagMaxNs.Load(),
	}
}

func observeDuration(total, max *atomic.Uint64, d time.Duration) {
	ns := uint64(d.Nanoseconds())
	total.Add(ns)
	observeMax(max, ns)
}

func observeMax(max *atomic.Uint64, value uint64) {
	for {
		current := max.Load()
		if value <= current || max.CompareAndSwap(current, value) {
			return
		}
	}
}

// StartReadLoop reads all incoming envelopes and dispatches responses
// to the appropriate pending channel by request_id. Handles every
// request/response type that flows through PluginConn: command,
// command hook, and operation hook. Blocks until the connection closes
// or Done is signalled — call from a goroutine.
func (pc *PluginConn) StartReadLoop() {
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

		pc.Deliver(reqID, env)
	}
}

func (pc *PluginConn) storePending(requestID string, ch chan *gcpc.EnvelopeV1) {
	pc.pendingMu.Lock()
	pc.pending.Store(requestID, ch)
	pc.pendingMu.Unlock()
}

// drainPending closes all pending channels so waiters unblock.
func (pc *PluginConn) drainPending() {
	pc.pendingMu.Lock()
	defer pc.pendingMu.Unlock()

	pc.pending.Range(func(key, value any) bool {
		close(value.(chan *gcpc.EnvelopeV1))
		pc.pending.Delete(key)
		return true
	})
}

// Deliver dispatches an envelope to the pending channel for the given
// request ID. Called by the manager's read loop to route response types
// (CommandResponse, HookResponse, OperationHookResponse) to their waiters.
func (pc *PluginConn) Deliver(requestID string, env *gcpc.EnvelopeV1) {
	pc.pendingMu.Lock()
	defer pc.pendingMu.Unlock()

	if ch, ok := pc.pending.LoadAndDelete(requestID); ok {
		ch.(chan *gcpc.EnvelopeV1) <- env
	}
}

// Close signals shutdown, drains pending channels, and closes the transport.
// Safe to call multiple times.
func (pc *PluginConn) Close() {
	pc.closeOnce.Do(func() {
		pc.sendMu.Lock()
		pc.closed = true
		close(pc.done)
		pc.sendMu.Unlock()

		if pc.conn != nil {
			_ = pc.conn.Close()
		}
		<-pc.writerDone
		pc.drainPending()
	})
}

// Done returns a channel that is closed when the connection is shut down.
func (pc *PluginConn) Done() <-chan struct{} {
	return pc.done
}

// DeletePending removes a pending request (used for cleanup on timeout).
func (pc *PluginConn) DeletePending(requestID string) {
	pc.pendingMu.Lock()
	pc.pending.Delete(requestID)
	pc.pendingMu.Unlock()
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

// PluginStats returns queue and writer measurements for every known plugin
// connection, sorted by plugin name for stable query output and tests.
func (r *Router) PluginStats() []PluginConnStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		names = append(names, name)
	}
	sort.Strings(names)

	stats := make([]PluginConnStats, 0, len(names))
	for _, name := range names {
		stats = append(stats, r.plugins[name].Stats())
	}
	return stats
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

// LookupMeta returns classification metadata for a registered plugin command
// without acquiring the full route connection. Returns nil if the command is
// not registered.
func (r *Router) LookupMeta(op string) *RouteMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, _, found := r.lookup(op)
	if !found {
		return nil
	}
	return &RouteMeta{ReadOnly: route.ReadOnly}
}

// Route dispatches a command to the owning plugin and waits for the response.
// Returns the result as an any compatible with evaluator.Result.Value.
// metadata carries REX metadata with bare keys (no shared.rex. prefix).
// conn carries the originating client connection info.
func (r *Router) Route(ctx context.Context, op string, args []string, metadata map[string]string, conn *gcpc.ConnectionInfoV1) (any, bool, error) {
	r.mu.RLock()
	route, pc, found := r.lookup(op)
	r.mu.RUnlock()

	if !found {
		return nil, false, ErrCommandNotFound
	}

	// Arg count validation.
	n := len(args)
	if n < route.MinArgs || (route.MaxArgs >= 0 && n > route.MaxArgs) {
		return nil, false, fmt.Errorf("ERR wrong number of arguments for '%s' command", strings.ToLower(op))
	}

	requestID := NextRequestID()
	var opCtx map[string]string
	if ctxOp := ops.FromContext(ctx); ctxOp != nil {
		opCtx = ctxOp.FilteredContext(route.PluginName, false)
	}
	cmd := &gcpc.CommandInfoV1{Name: route.Command, Args: args}
	env := gcpc.NewCommandRequest(requestID, cmd, conn, metadata, opCtx)

	respCh, err := pc.Send(ctx, env, requestID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ErrPluginTimeout
		}
		return nil, false, fmt.Errorf("%w: %s", ErrPluginDown, err.Error())
	}

	select {
	case resp, ok := <-respCh:
		if !ok {
			return nil, false, ErrPluginDown
		}
		cmdResp := resp.GetCommandResponse()
		if cmdResp == nil {
			return nil, false, ErrPluginDown
		}
		return gcpc.InterfaceFromResult(cmdResp.Result), cmdResp.SuppressResponse, nil
	case <-ctx.Done():
		pc.DeletePending(requestID)
		return nil, false, ErrPluginTimeout
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
