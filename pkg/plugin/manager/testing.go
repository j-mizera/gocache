package manager

import (
	"context"

	"gocache/commons/transport"
)

// TestSetCancel sets the lifecycle cancel func. Test-only.
func (m *Manager) TestSetCancel(cancel context.CancelFunc) {
	m.cancel = cancel
}

// TestAddInstance pre-registers a plugin entry. Test-only.
func (m *Manager) TestAddInstance(name string) {
	m.registry.Add(&PluginInstance{
		Name:        name,
		MaxRestarts: 0,
	})
}

// TestHandleConnection exposes handleConnection for cross-package tests.
func (m *Manager) TestHandleConnection(ctx context.Context, conn *transport.Conn) {
	m.handleConnection(ctx, conn)
}
