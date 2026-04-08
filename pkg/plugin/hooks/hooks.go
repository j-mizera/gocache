package hooks

import (
	"sort"
	"strings"
	"sync"

	"gocache/pkg/plugin/router"
	gcpc "gocache/proto/gcpc/v1"
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
	Critical   bool // critical = blocking, non-critical = fire-and-forget
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
type Registry struct {
	mu   sync.RWMutex
	pre  []*HookEntry // sorted by priority (lower first)
	post []*HookEntry // sorted by priority
}

// NewRegistry creates an empty hook registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds hooks declared by a plugin.
func (r *Registry) Register(pluginName string, priority int, critical bool, conn *router.PluginConn, decls []*gcpc.HookDeclV1) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, d := range decls {
		entry := &HookEntry{
			PluginName: pluginName,
			Pattern:    strings.ToUpper(strings.TrimSpace(d.Pattern)),
			Phase:      Phase(d.Phase),
			Critical:   critical,
			Priority:   priority,
			Conn:       conn,
		}
		switch entry.Phase {
		case PhasePre:
			r.pre = append(r.pre, entry)
		case PhasePost:
			r.post = append(r.post, entry)
		}
	}

	// Re-sort by priority after adding.
	sort.SliceStable(r.pre, func(i, j int) bool { return r.pre[i].Priority < r.pre[j].Priority })
	sort.SliceStable(r.post, func(i, j int) bool { return r.post[i].Priority < r.post[j].Priority })
}

// Unregister removes all hooks owned by the named plugin.
func (r *Registry) Unregister(pluginName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.pre = filterOut(r.pre, pluginName)
	r.post = filterOut(r.post, pluginName)
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

// HasAny returns true if any hooks are registered at all.
// Used as a zero-cost guard in the evaluator hot path.
func (r *Registry) HasAny() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pre) > 0 || len(r.post) > 0
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
