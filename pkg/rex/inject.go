package rex

// Prefix is the hook context key prefix for REX metadata.
// All REX metadata is injected under "shared.rex." so that all plugins
// can see it via the existing shared.* visibility in command.FilterHookCtx().
const Prefix = "shared.rex."

// BuildMetadata merges connection-scoped defaults and per-command metadata
// into a bare-key map (no shared.rex. prefix) suitable for GCPC metadata
// fields. Returns nil if no metadata exists.
func BuildMetadata(connStore *Store, cmdMeta map[string]string) map[string]string {
	var connLen int
	if connStore != nil {
		connStore.mu.RLock()
		connLen = len(connStore.data)
		connStore.mu.RUnlock()
	}
	if connLen == 0 && len(cmdMeta) == 0 {
		return nil
	}
	m := make(map[string]string, connLen+len(cmdMeta))
	if connStore != nil {
		connStore.mu.RLock()
		for k, v := range connStore.data {
			m[k] = v
		}
		connStore.mu.RUnlock()
	}
	for k, v := range cmdMeta {
		m[k] = v
	}
	return m
}

// InjectIntoHookCtx merges connection-scoped defaults and per-command
// metadata into the hook context map. Connection defaults are written first,
// then per-command overrides win.
func InjectIntoHookCtx(hookCtx map[string]string, connStore *Store, cmdMeta map[string]string) {
	// Connection defaults first.
	if connStore != nil {
		connStore.mu.RLock()
		for k, v := range connStore.data {
			hookCtx[Prefix+k] = v
		}
		connStore.mu.RUnlock()
	}

	// Per-command overrides win.
	for k, v := range cmdMeta {
		hookCtx[Prefix+k] = v
	}
}
