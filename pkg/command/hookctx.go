package command

import apicommand "gocache/api/command"

// Re-export hook context constants and helpers from api/command.
const (
	StartNs      = apicommand.StartNs
	ElapsedNs    = apicommand.ElapsedNs
	SharedPrefix = apicommand.SharedPrefix
)

var (
	NewHookCtx    = apicommand.NewHookCtx
	MergeHookCtx  = apicommand.MergeHookCtx
	FilterHookCtx = apicommand.FilterHookCtx
)
