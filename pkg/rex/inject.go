package rex

// Prefix is the hook context key prefix for REX metadata.
// All REX metadata is injected under "shared.rex." so that all plugins
// can see it via the existing shared.* visibility in command.FilterHookCtx().
const Prefix = "shared.rex."

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
