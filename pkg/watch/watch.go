package watch

import (
	"gocache/pkg/clientctx"
	"sync"
)

// Manager tracks which clients are watching which keys for optimistic locking.
// When a watched key is mutated, the watching client is marked dirty so that
// EXEC will abort the transaction.
type Manager struct {
	mu       sync.Mutex
	watchers map[string]map[*clientctx.ClientContext]struct{}
}

func NewManager() *Manager {
	return &Manager{
		watchers: make(map[string]map[*clientctx.ClientContext]struct{}),
	}
}

// Watch registers ctx as a watcher for each of the given keys.
func (m *Manager) Watch(ctx *clientctx.ClientContext, keys []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		if m.watchers[key] == nil {
			m.watchers[key] = make(map[*clientctx.ClientContext]struct{})
		}
		m.watchers[key][ctx] = struct{}{}
		ctx.WatchedKeys[key] = struct{}{}
	}
}

// Unwatch removes ctx from all watched keys and resets its watch state.
func (m *Manager) Unwatch(ctx *clientctx.ClientContext) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range ctx.WatchedKeys {
		if clients, ok := m.watchers[key]; ok {
			delete(clients, ctx)
			if len(clients) == 0 {
				delete(m.watchers, key)
			}
		}
	}
	ctx.ClearWatch()
}

// NotifyMutation marks all clients watching the given key as dirty.
func (m *Manager) NotifyMutation(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ctx := range m.watchers[key] {
		ctx.WatchDirty = true
	}
}

// NotifyAll marks every currently-watching client as dirty.
// Used for FLUSHDB/FLUSHALL which invalidate all keys.
func (m *Manager) NotifyAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, clients := range m.watchers {
		for ctx := range clients {
			ctx.WatchDirty = true
		}
	}
}
