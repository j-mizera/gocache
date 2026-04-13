package command

import opctx "gocache/api/context"

// Hook context constants. These are server-injected keys available to all
// plugins in the hook context map.
const (
	StartNs     = "_start_ns"     // Command start timestamp (nanoseconds since epoch)
	ElapsedNs   = "_elapsed_ns"   // Command execution duration (nanoseconds), post-hook only
	OperationID = "_operation_id" // Operation ID for correlation
)

// SharedPrefix is the key prefix for cross-plugin shared values.
const SharedPrefix = opctx.SharedPrefix

// Backward-compatible re-exports. New code should use api/context directly.
var (
	NewHookCtx    = opctx.NewContext
	MergeHookCtx  = opctx.MergeFromPlugin
	FilterHookCtx = opctx.FilterForPlugin
)
