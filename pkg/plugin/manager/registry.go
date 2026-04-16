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
//
// Lifecycle mutation happens from multiple goroutines (launchPlugin,
// handleConnection, healthLoop, readLoop, handlePluginExit, Shutdown) so
// all mutable fields are guarded by mu. The immutable configuration fields
// (Name, BinPath, Priority, MaxRestarts) are set at construction and never
// modified, so they are exported and safe to read without locking.
type PluginInstance struct {
	// Immutable after construction — safe to read without locking.
	Name        string
	BinPath     string
	Priority    int
	MaxRestarts int

	// mu guards all fields below.
	mu            sync.RWMutex
	version       string
	critical      bool
	state         PluginState
	conn          *transport.Conn
	cmd           *exec.Cmd
	lastHealth    time.Time
	restarts      int
	commands      []*gcpc.CommandDeclV1
	grantedScopes []string
}

// State returns the plugin's current lifecycle state.
func (p *PluginInstance) State() PluginState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// SetState assigns the plugin's current lifecycle state.
func (p *PluginInstance) SetState(s PluginState) {
	p.mu.Lock()
	p.state = s
	p.mu.Unlock()
}

// Conn returns the transport connection, or nil if not yet registered or already closed.
func (p *PluginInstance) Conn() *transport.Conn {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.conn
}

// SetConn assigns the transport connection.
func (p *PluginInstance) SetConn(c *transport.Conn) {
	p.mu.Lock()
	p.conn = c
	p.mu.Unlock()
}

// Cmd returns the underlying process handle, or nil if not yet started.
func (p *PluginInstance) Cmd() *exec.Cmd {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cmd
}

// SetCmd assigns the underlying process handle.
func (p *PluginInstance) SetCmd(c *exec.Cmd) {
	p.mu.Lock()
	p.cmd = c
	p.mu.Unlock()
}

// Version returns the version string the plugin reported at registration.
func (p *PluginInstance) Version() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.version
}

// Critical reports whether a failure of this plugin should crash the server.
func (p *PluginInstance) Critical() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.critical
}

// SetCritical overrides the critical flag. Normally set at construction from
// YAML or self-declared during registration — this setter exists for that single
// registration-time override path.
func (p *PluginInstance) SetCritical(c bool) {
	p.mu.Lock()
	p.critical = c
	p.mu.Unlock()
}

// SetLastHealth records the time of the most recent successful health check.
func (p *PluginInstance) SetLastHealth(t time.Time) {
	p.mu.Lock()
	p.lastHealth = t
	p.mu.Unlock()
}

// Restarts returns the number of restart attempts so far.
func (p *PluginInstance) Restarts() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.restarts
}

// IncrementRestarts bumps the restart counter and returns the new value.
func (p *PluginInstance) IncrementRestarts() int {
	p.mu.Lock()
	p.restarts++
	n := p.restarts
	p.mu.Unlock()
	return n
}

// GrantedScopes returns a snapshot of the scopes granted at registration.
func (p *PluginInstance) GrantedScopes() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.grantedScopes))
	copy(out, p.grantedScopes)
	return out
}

// Register atomically applies the registration payload to the instance.
// Called once on handshake completion; serialises the version/commands/scopes
// assignment so concurrent readers never observe a half-initialised instance.
// criticalOverride is applied only if honor is true (i.e. no YAML override).
func (p *PluginInstance) Register(
	conn *transport.Conn,
	version string,
	commands []*gcpc.CommandDeclV1,
	grantedScopes []string,
	criticalOverride bool,
	honorCritical bool,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn = conn
	p.version = version
	p.commands = commands
	p.grantedScopes = grantedScopes
	if honorCritical {
		p.critical = criticalOverride
	}
}

// setCriticalAtLoad is used by the manager during discovery to seed critical
// from YAML/discovery before Register runs. Package-private because it has no
// valid external use; external "override" paths should use SetCritical.
func (p *PluginInstance) setCriticalAtLoad(c bool) {
	p.critical = c
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

// SetState updates the state of a named plugin via the instance's own mutex.
// Kept for API compatibility; prefer PluginInstance.SetState when you already
// hold a reference to the instance.
func (r *Registry) SetState(name string, state PluginState) {
	r.mu.RLock()
	p, ok := r.plugins[name]
	r.mu.RUnlock()
	if ok {
		p.SetState(state)
	}
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.plugins)
}
