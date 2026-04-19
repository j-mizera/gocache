package command

import opctx "gocache/api/context"

// Hook and operation context constants. These are server-injected keys
// available to all plugins in the hook context map and on operation
// context snapshots. All keys start with "_" to mark them as server-owned
// (see api/context FilterForPlugin — server keys are visible to every
// plugin but not writable by them).
const (
	// Timing keys — injected by the server around command execution.
	StartNs   = "_start_ns"   // Start timestamp (nanoseconds since epoch)
	ElapsedNs = "_elapsed_ns" // Execution duration (nanoseconds), post-hook only

	// Correlation keys — propagated across operations, events, and logs.
	OperationID = "_operation_id" // Operation ID for cross-signal correlation

	// Command enrichment keys — set by the evaluator on command operations.
	CommandKey  = "_command"   // Uppercased RESP command name
	ArgCountKey = "_arg_count" // Number of command arguments (stringified int)
	ResultKey   = "_result"    // Serialized command result (post-execution)
	ErrorKey    = "_error"     // Error message, empty if command succeeded

	// Worker / lifecycle enrichment keys.
	TriggerKey = "_trigger" // What initiated the operation ("scheduled", "startup", ...)
	FileKey    = "_file"    // File path associated with the operation

	// Connection enrichment keys.
	RemoteAddrKey = "_remote_addr" // Remote peer address for connection operations

	// Log collector field names.
	CtxField = "_ctx" // Nested operation-context object in JSON log lines
)

// SharedPrefix is the key prefix for cross-plugin shared values.
const SharedPrefix = opctx.SharedPrefix

// Backward-compatible re-exports. New code should use api/context directly.
var (
	NewHookCtx    = opctx.NewContext
	MergeHookCtx  = opctx.MergeFromPlugin
	FilterHookCtx = opctx.FilterForPlugin
)
