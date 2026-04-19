package command

import apicommand "gocache/api/command"

// Re-export hook context and operation enrichment constants from api/command.
const (
	StartNs      = apicommand.StartNs
	ElapsedNs    = apicommand.ElapsedNs
	SharedPrefix = apicommand.SharedPrefix
	OperationID  = apicommand.OperationID

	CommandKey    = apicommand.CommandKey
	ArgCountKey   = apicommand.ArgCountKey
	ResultKey     = apicommand.ResultKey
	ErrorKey      = apicommand.ErrorKey
	TriggerKey    = apicommand.TriggerKey
	FileKey       = apicommand.FileKey
	RemoteAddrKey = apicommand.RemoteAddrKey
	CtxField      = apicommand.CtxField
)

var (
	NewHookCtx    = apicommand.NewHookCtx
	MergeHookCtx  = apicommand.MergeHookCtx
	FilterHookCtx = apicommand.FilterHookCtx
)
