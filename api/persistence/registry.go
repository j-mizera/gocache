package persistence

import (
	"context"
	"fmt"
	"sync"

	apicommand "gocache/api/command"
	apiconfig "gocache/api/config"
)

// Provider is the registration handle for an embedded persistence
// plugin. Each plugin's init() calls RegisterProvider; cmd/server
// discovers all registered providers via RegisteredProviders and
// builds them generically — it never knows (or cares) what specific
// implementation is behind a provider.
//
// Multiple providers may coexist: an AOF sink + a snapshot writer +
// a custom replication sink are all valid simultaneously. The server
// collects Sources, Sinks, and Snapshotters from every Backend and
// wires them into the coordinator without special-casing any provider.
type Provider interface {
	Name() string
	Build(cfg apiconfig.PluginConfig, store CacheStore) (*Backend, error)
}

// Backend is the result of Provider.Build. Every field is optional —
// a plugin implements only what it needs (granular interfaces, no
// god-struct forcing stubs).
type Backend struct {
	Source      Source
	Sink        Sink
	Snapshotter Snapshotter

	// Commands returns the commands this plugin wants to register.
	// Called after the persistence coordinator is ready, so handlers
	// can use the PersistenceAPI to trigger snapshots or query state.
	// Nil means "no commands."
	Commands func(api PersistenceAPI) []Command

	// OnReload is called on config hot-reload events. Nil means the
	// plugin has no runtime-tunable knobs.
	OnReload apiconfig.ReloadHandler
}

// PersistenceAPI is the server-side persistence coordinator's public
// surface, exposed to plugin command handlers so they can trigger
// snapshots or query coordinator state without importing pkg/.
type PersistenceAPI interface {
	Snapshot(ctx context.Context) error
	LastSaveUnix() int64
}

// Command is a RESP command registered by a persistence plugin.
type Command struct {
	Name string
	Fn   CommandHandler
	Spec apicommand.Spec
}

// CommandHandler handles a plugin-registered RESP command. Returns a
// value that the pipeline marshals to RESP wire format:
//   - string  → simple string
//   - int64   → integer
//   - []byte  → bulk string
//   - nil     → null
//   - error   → RESP error (via second return)
type CommandHandler func(ctx context.Context, args []string) (any, error)

var (
	providersMu sync.RWMutex
	providers   []Provider
)

// RegisterProvider installs an embedded persistence plugin. Called from
// the plugin's init(). Multiple providers may register — the server
// builds all of them. Duplicate names panic so misconfiguration
// surfaces at startup.
func RegisterProvider(p Provider) {
	if p == nil {
		panic("api/persistence: RegisterProvider called with nil")
	}
	providersMu.Lock()
	defer providersMu.Unlock()
	for _, existing := range providers {
		if existing.Name() == p.Name() {
			panic(fmt.Sprintf(
				"api/persistence: provider %q already registered",
				p.Name(),
			))
		}
	}
	providers = append(providers, p)
}

// RegisteredProviders returns all registered providers. Returns nil
// when no plugins were compiled in.
func RegisteredProviders() []Provider {
	providersMu.RLock()
	defer providersMu.RUnlock()
	if len(providers) == 0 {
		return nil
	}
	out := make([]Provider, len(providers))
	copy(out, providers)
	return out
}

// ResetProvidersForTest clears all registered providers. Test-only.
func ResetProvidersForTest() {
	providersMu.Lock()
	defer providersMu.Unlock()
	providers = nil
}
