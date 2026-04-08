package clientctx

import "errors"

var (
	ErrNestedMulti         = errors.New("multi calls cannot be nested")
	ErrDiscardWithoutMulti = errors.New("discard without multi")
	ErrExecWithoutMulti    = errors.New("exec without multi")
)

type ClientContext struct {
	InTransaction bool
	CommandQueue  [][]string
	ProtoVersion  int
	Authenticated bool
	WatchedKeys   map[string]struct{}
	WatchDirty    bool
}

func New() *ClientContext {
	return &ClientContext{
		CommandQueue: make([][]string, 0),
		ProtoVersion: 2,
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
