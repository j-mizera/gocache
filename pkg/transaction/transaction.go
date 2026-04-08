package transaction

import (
	"gocache/pkg/clientctx"
)

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) Multi(ctx *clientctx.ClientContext) (string, error) {
	if ctx.InTransaction {
		return "", clientctx.ErrNestedMulti
	}
	ctx.StartTransaction()
	return "OK", nil
}

func (m *Manager) Discard(ctx *clientctx.ClientContext) (string, error) {
	if !ctx.InTransaction {
		return "", clientctx.ErrDiscardWithoutMulti
	}
	ctx.ResetTransaction()
	return "OK", nil
}
