package clientctx

import (
	"errors"

	"gocache/pkg/rex"
)

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
	WatchDirty    bool
	RexVersion    int               // 0 = disabled, 1 = META lines enabled
	RexMeta       *rex.Store        // nil until first REX.META SET/MSET
	CmdMeta       map[string]string // transient per-command META, set by server, cleared after eval
	OperationID   string            // parent operation ID (connection operation), set by server
}

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
	c.WatchDirty = false
}
