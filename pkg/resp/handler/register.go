package handler

import (
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// reg is shorthand for building a Registration inline.
func reg(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: command.Spec{Min: min, Max: max}}
}

// Registrations returns all RESP command handlers with their argument specs.
func Registrations() map[string]command.Registration {
	return map[string]command.Registration{
		// String commands
		resp.CmdSet:     reg(HandleSet, 2, -1),
		resp.CmdGet:     reg(HandleGet, 1, 1),
		resp.CmdDelete:  reg(HandleDelete, 1, -1),
		resp.CmdExists:  reg(HandleExists, 1, 1),
		resp.CmdExpire:  reg(HandleExpire, 2, 2),
		resp.CmdPExpire: reg(HandlePexpire, 2, 2),
		resp.CmdTTL:     reg(HandleTtl, 1, 1),
		resp.CmdPTTL:    reg(HandlePttl, 1, 1),
		resp.CmdSetNX:   reg(HandleSetnx, 2, 2),

		// List commands
		resp.CmdLPush:  reg(HandleLpush, 2, -1),
		resp.CmdRPush:  reg(HandleRpush, 2, -1),
		resp.CmdLPop:   reg(HandleLpop, 1, 1),
		resp.CmdRPop:   reg(HandleRpop, 1, 1),
		resp.CmdLLen:   reg(HandleLlen, 1, 1),
		resp.CmdLRange: reg(HandleLrange, 3, 3),
		resp.CmdBLPop:  reg(HandleBlpop, 2, -1),
		resp.CmdBRPop:  reg(HandleBrpop, 2, -1),

		// Hash commands
		resp.CmdHSet:    reg(HandleHset, 3, -1),
		resp.CmdHGet:    reg(HandleHget, 2, 2),
		resp.CmdHDel:    reg(HandleHdel, 2, -1),
		resp.CmdHExists: reg(HandleHexists, 2, 2),
		resp.CmdHGetAll: reg(HandleHgetall, 1, 1),
		resp.CmdHKeys:   reg(HandleHkeys, 1, 1),
		resp.CmdHVals:   reg(HandleHvals, 1, 1),
		resp.CmdHLen:    reg(HandleHlen, 1, 1),

		// Set commands
		resp.CmdSAdd:      reg(HandleSadd, 2, -1),
		resp.CmdSRem:      reg(HandleSrem, 2, -1),
		resp.CmdSMembers:  reg(HandleSmembers, 1, 1),
		resp.CmdSIsMember: reg(HandleSismember, 2, 2),
		resp.CmdSCard:     reg(HandleScard, 1, 1),
		resp.CmdSPop:      reg(HandleSpop, 1, 1),
		resp.CmdSInter:    reg(HandleSinter, 1, -1),
		resp.CmdSUnion:    reg(HandleSunion, 1, -1),
		resp.CmdSDiff:     reg(HandleSdiff, 1, -1),

		// Sorted Set commands
		resp.CmdZAdd:   reg(HandleZadd, 3, -1),
		resp.CmdZRem:   reg(HandleZrem, 2, -1),
		resp.CmdZScore: reg(HandleZscore, 2, 2),
		resp.CmdZCard:  reg(HandleZcard, 1, 1),
		resp.CmdZRange: reg(HandleZrange, 3, 4),
		resp.CmdZRank:  reg(HandleZrank, 2, 2),
		resp.CmdZCount: reg(HandleZcount, 3, 3),

		// Transaction commands
		resp.CmdMulti:   reg(HandleMulti, 0, 0),
		resp.CmdDiscard: reg(HandleDiscard, 0, 0),
		resp.CmdExec:    reg(HandleExec, 0, 0),

		// Persistence commands
		resp.CmdSnapshot:     reg(HandleSnapshot, 0, 0),
		resp.CmdLoadSnapshot: reg(HandleLoadSnapshot, 1, 1),

		// Server commands
		resp.CmdDBSize:   reg(HandleDbsize, 0, 0),
		resp.CmdInfo:     reg(HandleInfo, 0, 1),
		resp.CmdHello:    reg(HandleHello, 1, -1),
		resp.CmdPing:     reg(HandlePing, 0, 1),
		resp.CmdEcho:     reg(HandleEcho, 1, 1),
		resp.CmdSelect:   reg(HandleSelect, 1, 1),
		resp.CmdFlushDB:  reg(HandleFlushDB, 0, 0),
		resp.CmdFlushAll: reg(HandleFlushAll, 0, 0),
		resp.CmdAuth:     reg(HandleAuth, 1, 1),

		// String counter commands
		resp.CmdIncr:        reg(HandleIncr, 1, 1),
		resp.CmdDecr:        reg(HandleDecr, 1, 1),
		resp.CmdIncrBy:      reg(HandleIncrBy, 2, 2),
		resp.CmdDecrBy:      reg(HandleDecrBy, 2, 2),
		resp.CmdIncrByFloat: reg(HandleIncrByFloat, 2, 2),
		resp.CmdAppend:      reg(HandleAppend, 2, 2),
		resp.CmdStrlen:      reg(HandleStrlen, 1, 1),

		// Multi-key commands
		resp.CmdMGet: reg(HandleMget, 1, -1),
		resp.CmdMSet: reg(HandleMset, 2, -1),

		// Key management commands
		resp.CmdType:      reg(HandleType, 1, 1),
		resp.CmdRename:    reg(HandleRename, 2, 2),
		resp.CmdRenameNX:  reg(HandleRenameNX, 2, 2),
		resp.CmdKeys:      reg(HandleKeys, 1, 1),
		resp.CmdScan:      reg(HandleScan, 1, -1),
		resp.CmdRandomKey: reg(HandleRandomKey, 0, 0),

		// Watch commands
		resp.CmdWatch:   reg(HandleWatch, 1, -1),
		resp.CmdUnwatch: reg(HandleUnwatch, 0, 0),

		// Key introspection
		resp.CmdObject: reg(HandleObject, 1, 2),
	}
}
