package manager

import (
	"fmt"
	"os/exec"
	"sync"
	"time"

	gcpc "gocache/api/gcpc/v1"
	"gocache/api/transport"
)

// PluginState represents the current lifecycle state of a plugin.
type PluginState int

const (
	StateLoaded     PluginState = iota // config scanned
	StateStarting                      // process fork/exec'd
	StateConnected                     // UDS connection established
	StateRegistered                    // REGISTER received and accepted
	StateRunning                       // health-check loop active
	StateUnhealthy                     // health check failed
	StateRestarting                    // non-critical restart in progress
	StateShutdown                      // shutdown complete
)

func (s PluginState) String() string {
	switch s {
	case StateLoaded:
		return "loaded"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateRegistered:
		return "registered"
	case StateRunning:
		return "running"
	case StateUnhealthy:
		return "unhealthy"
	case StateRestarting:
		return "restarting"
	case StateShutdown:
		return "shutdown"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// PluginInstance holds runtime state for a single plugin.
type PluginInstance struct {
	Name          string
	Version       string
	Critical      bool
	Priority      int
	BinPath       string
	State         PluginState
	Conn          *transport.Conn
	Cmd           *exec.Cmd
	LastHealth    time.Time
	Restarts      int
	MaxRestarts   int
	Commands      []*gcpc.CommandDeclV1 // commands declared during registration
	GrantedScopes []string              // scopes granted during registration
}

// Registry is a thread-safe catalog of plugin instances.
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]*PluginInstance
}

func NewRegistry() *Registry {
	return &Registry{
		plugins: make(map[string]*PluginInstance),
	}
}

func (r *Registry) Add(p *PluginInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins[p.Name] = p
}

func (r *Registry) Get(name string) (*PluginInstance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[name]
	return p, ok
}

func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.plugins, name)
}

func (r *Registry) All() []*PluginInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*PluginInstance, 0, len(r.plugins))
	for _, p := range r.plugins {
		result = append(result, p)
	}
	return result
}

func (r *Registry) SetState(name string, state PluginState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.plugins[name]; ok {
		p.State = state
	}
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.plugins)
}
