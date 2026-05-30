package cmdhooks

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	gcpc "gocache/api/gcpc/v1"
	"gocache/pkg/plugin/router"
)

// Phase indicates when a hook fires relative to command execution.
type Phase int

const (
	PhasePre  Phase = 1
	PhasePost Phase = 2
)

// HookEntry is a single registered hook from a plugin.
type HookEntry struct {
	PluginName string
	Pattern    string // uppercase, "*" = wildcard
	Phase      Phase
	Blocking   bool // true = server waits for response and can honour deny
	Critical   bool // true = hook error/timeout fails the command; false = fail-open
	Priority   int  // lower = higher priority
	Conn       *router.PluginConn
}

// matches returns true if this hook matches the given command.
func (h *HookEntry) matches(command string) bool {
	if h.Pattern == "*" {
		return true
	}
	return h.Pattern == command
}

// Registry stores all registered hooks, indexed for fast lookup.
//
// total mirrors len(pre)+len(post) for the lock-free HasAny check on the
// evaluator hot path. Updated under mu; read with atomic.Load so the per-
// command guard does not pay RLock cost.
type Registry struct {
	mu    sync.RWMutex
	pre   []*HookEntry // sorted by priority (lower first)
	post  []*HookEntry // sorted by priority
	total atomic.Int32
}

// NewRegistry creates an empty hook registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds hooks declared by a plugin.
// pluginCritical is the plugin-level critical flag, used as fallback when the
// hook declaration does not set its own critical field.
func (r *Registry) Register(pluginName string, priority int, pluginCritical bool, conn *router.PluginConn, decls []*gcpc.HookDeclV1) {
	r.mu.Lock()
	defer r.mu.Unlock()

	added := 0
	for _, d := range decls {
		hookCritical := pluginCritical
		if d.Critical != nil {
			hookCritical = *d.Critical
		}
		entry := &HookEntry{
			PluginName: pluginName,
			Pattern:    strings.ToUpper(strings.TrimSpace(d.Pattern)),
			Phase:      Phase(d.Phase),
			Blocking:   d.Blocking,
			Critical:   hookCritical,
			Priority:   priority,
			Conn:       conn,
		}
		switch entry.Phase {
		case PhasePre:
			r.pre = append(r.pre, entry)
			added++
		case PhasePost:
			r.post = append(r.post, entry)
			added++
		}
	}

	if added > 0 {
		r.total.Add(int32(added))
	}

	// Re-sort by priority after adding.
	sort.SliceStable(r.pre, func(i, j int) bool { return r.pre[i].Priority < r.pre[j].Priority })
	sort.SliceStable(r.post, func(i, j int) bool { return r.post[i].Priority < r.post[j].Priority })
}

// ConnFor returns the first connection registered for a plugin. nil if the
// plugin has no command hooks registered.
func (r *Registry) ConnFor(pluginName string) *router.PluginConn {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, h := range r.pre {
		if h.PluginName == pluginName {
			return h.Conn
		}
	}
	for _, h := range r.post {
		if h.PluginName == pluginName {
			return h.Conn
		}
	}
	return nil
}

// Unregister removes all hooks owned by the named plugin.
func (r *Registry) Unregister(pluginName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	before := len(r.pre) + len(r.post)
	r.pre = filterOut(r.pre, pluginName)
	r.post = filterOut(r.post, pluginName)
	removed := before - len(r.pre) - len(r.post)
	if removed > 0 {
		r.total.Add(int32(-removed))
	}
}

// MatchPre returns matching pre-hooks for the given command, priority-sorted.
// Returns nil if no hooks match.
func (r *Registry) MatchPre(command string) []*HookEntry {
	return r.match(command, true)
}

// MatchPost returns matching post-hooks for the given command, priority-sorted.
// Returns nil if no hooks match.
func (r *Registry) MatchPost(command string) []*HookEntry {
	return r.match(command, false)
}

// HasAny returns true if any hooks are registered at all. Implemented as an
// atomic load — the evaluator calls this on every command, so RLock cost
// would dominate the path it gates.
func (r *Registry) HasAny() bool {
	return r.total.Load() > 0
}

// HasCommand reports whether any pre or post hook matches command. It lets
// the evaluator keep commands on the fast path when hooks exist only for
// unrelated commands.
func (r *Registry) HasCommand(command string) bool {
	if !r.HasAny() {
		return false
	}
	command = strings.ToUpper(command)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.pre {
		if h.matches(command) {
			return true
		}
	}
	for _, h := range r.post {
		if h.matches(command) {
			return true
		}
	}
	return false
}

func (r *Registry) match(command string, pre bool) []*HookEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	command = strings.ToUpper(command)
	var source []*HookEntry
	if pre {
		source = r.pre
	} else {
		source = r.post
	}

	var result []*HookEntry
	for _, h := range source {
		if h.matches(command) {
			result = append(result, h)
		}
	}
	return result
}

func filterOut(entries []*HookEntry, pluginName string) []*HookEntry {
	n := 0
	for _, e := range entries {
		if e.PluginName != pluginName {
			entries[n] = e
			n++
		}
	}
	return entries[:n]
}
