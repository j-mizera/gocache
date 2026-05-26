package clientctx

import (
	"errors"
	"fmt"
	"sync/atomic"

	"gocache/pkg/rex"
)

var connSeq atomic.Uint64

// NextConnectionID returns a unique connection identifier, independent of
// the operation tracking system.
func NextConnectionID() string {
	return fmt.Sprintf("cid_%d", connSeq.Add(1))
}

var (
	ErrNestedMulti         = errors.New("multi calls cannot be nested")
	ErrDiscardWithoutMulti = errors.New("discard without multi")
	ErrExecWithoutMulti    = errors.New("exec without multi")
)

// defaultProtoVersion is the initial RESP protocol version for a new client
// connection. Clients upgrade to RESP3 via the HELLO command.
const defaultProtoVersion = 2

type ClientContext struct {
	InTransaction bool
	CommandQueue  [][]string
	ProtoVersion  int
	Authenticated bool
	WatchedKeys   map[string]struct{}
	// watchDirty is set from another connection's mutation goroutine via
	// watch.Manager.NotifyMutation while held by the engine's cache lock,
	// and read by HandleExec on this connection's goroutine without that
	// lock. Atomic access bridges the cross-goroutine boundary.
	watchDirty    atomic.Bool
	RexVersion    int               // 0 = disabled, 1 = META lines enabled
	RexMeta       *rex.Store        // nil until first REX.META SET/MSET
	CmdMeta       map[string]string // transient per-command META, set by server, cleared after eval
	ConnectionID  string            // stable connection identifier, independent of operations
	RemoteAddr    string            // remote peer address, set by server
	OperationID   string            // parent operation ID (connection operation), set by server
}

// IsWatchDirty reports whether a mutation has invalidated this client's
// watched keys. Safe to call from any goroutine.
func (c *ClientContext) IsWatchDirty() bool { return c.watchDirty.Load() }

// MarkWatchDirty flips the dirty flag. Idempotent; multiple concurrent
// callers settle on true.
func (c *ClientContext) MarkWatchDirty() { c.watchDirty.Store(true) }

// ClearWatchDirty resets the dirty flag (called from EXEC/DISCARD).
func (c *ClientContext) ClearWatchDirty() { c.watchDirty.Store(false) }

func New() *ClientContext {
	return &ClientContext{
		CommandQueue: make([][]string, 0),
		ProtoVersion: defaultProtoVersion,
		WatchedKeys:  make(map[string]struct{}),
	}
}

func (c *ClientContext) ResetTransaction() {
	c.InTransaction = false
	c.CommandQueue = nil
}

func (c *ClientContext) StartTransaction() {
	c.InTransaction = true
	c.CommandQueue = make([][]string, 0)
}

func (c *ClientContext) EnqueueCommand(parts []string) {
	c.CommandQueue = append(c.CommandQueue, parts)
}

// ClearWatch resets all watch state on this client.
func (c *ClientContext) ClearWatch() {
	c.WatchedKeys = make(map[string]struct{})
	c.watchDirty.Store(false)
}
