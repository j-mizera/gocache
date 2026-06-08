package observability

import (
	"sync/atomic"

	apiobs "gocache/api/observability"
)

// ConnectionContext is an owner-side pointer to the current immutable
// connection-context version. The owning connection goroutine publishes new
// versions through UpdateOwnedConnectionContext*; readers may pin the published
// version concurrently without racing on the owner field.
type ConnectionContext struct {
	current atomic.Uint64
}

func (c *ConnectionContext) load() apiobs.ConnectionContextVersion {
	if c == nil {
		return 0
	}
	return apiobs.ConnectionContextVersion(c.current.Load())
}

func (c *ConnectionContext) store(version apiobs.ConnectionContextVersion) {
	if c == nil {
		return
	}
	c.current.Store(uint64(version))
}

// UpdateOwnedConnectionContextStrings updates the manager-owned immutable
// context store and publishes the resulting version through owner.
func (m *SlotOperationTrackerManager) UpdateOwnedConnectionContextStrings(owner *ConnectionContext, connection apiobs.ConnectionIdentity, pairs ...string) apiobs.ConnectionContextVersion {
	version := m.UpdateConnectionContextStrings(connection, pairs...)
	owner.store(version)
	return version
}

// RemoveOwnedConnectionContextStrings removes keys from the current connection
// context and publishes the resulting version through owner.
func (m *SlotOperationTrackerManager) RemoveOwnedConnectionContextStrings(owner *ConnectionContext, connection apiobs.ConnectionIdentity, keys ...string) apiobs.ConnectionContextVersion {
	version := m.RemoveConnectionContextStrings(connection, keys...)
	if !version.IsZero() {
		owner.store(version)
	}
	return version
}

// PinOwnedConnectionContextVersion retains the version currently published by
// owner. It returns zero if the owner has no version or if a concurrent update
// already reclaimed the observed version before it could be retained.
func (m *SlotOperationTrackerManager) PinOwnedConnectionContextVersion(owner *ConnectionContext) apiobs.ConnectionContextVersion {
	version := owner.load()
	if version.IsZero() {
		return 0
	}
	if !m.RetainConnectionContextVersion(version) {
		return 0
	}
	return version
}

// ReclaimConnectionContextVersions removes unreferenced non-current versions.
// The current store already reclaims eagerly on release/update, but keeping this
// explicit hook lets callers/tests exercise the intended janitor boundary.
func (m *SlotOperationTrackerManager) ReclaimConnectionContextVersions() {
	m.contexts.reclaim()
}

// UpdateOwnedConnectionContextStrings updates the sharded manager's immutable
// context store and publishes the resulting version through owner.
func (m *ShardedOperationTrackerManager) UpdateOwnedConnectionContextStrings(owner *ConnectionContext, connection apiobs.ConnectionIdentity, pairs ...string) apiobs.ConnectionContextVersion {
	version := m.UpdateConnectionContextStrings(connection, pairs...)
	owner.store(version)
	return version
}

// RemoveOwnedConnectionContextStrings removes keys from the sharded manager's
// current connection context and publishes the resulting version through owner.
func (m *ShardedOperationTrackerManager) RemoveOwnedConnectionContextStrings(owner *ConnectionContext, connection apiobs.ConnectionIdentity, keys ...string) apiobs.ConnectionContextVersion {
	version := m.RemoveConnectionContextStrings(connection, keys...)
	if !version.IsZero() {
		owner.store(version)
	}
	return version
}

// PinOwnedConnectionContextVersion retains the version currently published by
// owner in the sharded manager.
func (m *ShardedOperationTrackerManager) PinOwnedConnectionContextVersion(owner *ConnectionContext) apiobs.ConnectionContextVersion {
	version := owner.load()
	if version.IsZero() {
		return 0
	}
	if !m.RetainConnectionContextVersion(version) {
		return 0
	}
	return version
}

// ReclaimConnectionContextVersions removes unreferenced non-current versions.
func (m *ShardedOperationTrackerManager) ReclaimConnectionContextVersions() {
	m.contexts.reclaim()
}
