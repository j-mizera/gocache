package handler

import (
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// reg is shorthand for building a Registration inline.
func reg(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: command.Spec{Min: min, Max: max}}
}

// regRO is reg with Spec.ReadOnly = true. Use only for handlers that
// strictly read from the cache and never mutate (no SET, no DEL, no
// expiration update, no LRU mutation, no WATCH dirty-bit propagation).
// Read-like commands with side effects (GETSET, GETDEL, GETEX, BLPOP,
// LPOP, SPOP, SRANDMEMBER-with-count-equal-to-cardinality) must use reg.
func regRO(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: command.Spec{Min: min, Max: max, ReadOnly: true}}
}

// Registrations returns all RESP command handlers with their argument specs.
func Registrations() map[string]command.Registration {
	return map[string]command.Registration{
		// String commands
		resp.CmdSet:     reg(HandleSet, 2, -1),
		resp.CmdGet:     regRO(HandleGet, 1, 1),
		resp.CmdDelete:  reg(HandleDelete, 1, -1),
		resp.CmdExists:  regRO(HandleExists, 1, 1),
		resp.CmdExpire:  reg(HandleExpire, 2, 2),
		resp.CmdPExpire: reg(HandlePexpire, 2, 2),
		resp.CmdTTL:     regRO(HandleTtl, 1, 1),
		resp.CmdPTTL:    regRO(HandlePttl, 1, 1),
		resp.CmdSetNX:   reg(HandleSetnx, 2, 2),

		// List commands
		resp.CmdLPush:  reg(HandleLpush, 2, -1),
		resp.CmdRPush:  reg(HandleRpush, 2, -1),
		resp.CmdLPop:   reg(HandleLpop, 1, 1),
		resp.CmdRPop:   reg(HandleRpop, 1, 1),
		resp.CmdLLen:   regRO(HandleLlen, 1, 1),
		resp.CmdLRange: regRO(HandleLRange, 3, 3),
		resp.CmdBLPop:  reg(HandleBlpop, 2, -1),
		resp.CmdBRPop:  reg(HandleBrpop, 2, -1),

		// Hash commands
		resp.CmdHSet:    reg(HandleHset, 3, -1),
		resp.CmdHGet:    regRO(HandleHget, 2, 2),
		resp.CmdHDel:    reg(HandleHdel, 2, -1),
		resp.CmdHExists: regRO(HandleHexists, 2, 2),
		resp.CmdHGetAll: regRO(HandleHgetall, 1, 1),
		resp.CmdHKeys:   regRO(HandleHkeys, 1, 1),
		resp.CmdHVals:   regRO(HandleHvals, 1, 1),
		resp.CmdHLen:    regRO(HandleHlen, 1, 1),

		// Set commands
		resp.CmdSAdd:      reg(HandleSadd, 2, -1),
		resp.CmdSRem:      reg(HandleSrem, 2, -1),
		resp.CmdSMembers:  regRO(HandleSmembers, 1, 1),
		resp.CmdSIsMember: regRO(HandleSismember, 2, 2),
		resp.CmdSCard:     regRO(HandleScard, 1, 1),
		resp.CmdSPop:      reg(HandleSpop, 1, 1),
		resp.CmdSInter:    regRO(HandleSinter, 1, -1),
		resp.CmdSUnion:    regRO(HandleSunion, 1, -1),
		resp.CmdSDiff:     regRO(HandleSdiff, 1, -1),

		// Sorted Set commands
		resp.CmdZAdd:   reg(HandleZadd, 3, -1),
		resp.CmdZRem:   reg(HandleZrem, 2, -1),
		resp.CmdZScore: regRO(HandleZscore, 2, 2),
		resp.CmdZCard:  regRO(HandleZcard, 1, 1),
		resp.CmdZRange: regRO(HandleZrange, 3, 4),
		resp.CmdZRank:  regRO(HandleZrank, 2, 2),
		resp.CmdZCount: regRO(HandleZcount, 3, 3),

		// Transaction commands — never read-only: MULTI/DISCARD/EXEC
		// transition client state and EXEC re-enters the engine.
		resp.CmdMulti:   reg(HandleMulti, 0, 0),
		resp.CmdDiscard: reg(HandleDiscard, 0, 0),
		resp.CmdExec:    reg(HandleExec, 0, 0),

		// Persistence commands — Snapshot writes the snapshot file (side
		// effect even though no cache mutation); LoadSnapshot mutates state.
		resp.CmdSnapshot:     reg(HandleSnapshot, 0, 0),
		resp.CmdLoadSnapshot: reg(HandleLoadSnapshot, 1, 1),

		// Server commands
		resp.CmdDBSize:   regRO(HandleDBSize, 0, 0),
		resp.CmdInfo:     regRO(HandleInfo, 0, 1),
		resp.CmdHello:    reg(HandleHello, 1, -1), // mutates client proto-version state
		resp.CmdPing:     regRO(HandlePing, 0, 1),
		resp.CmdEcho:     regRO(HandleEcho, 1, 1),
		resp.CmdSelect:   reg(HandleSelect, 1, 1),
		resp.CmdFlushDB:  reg(HandleFlushDB, 0, 0),
		resp.CmdFlushAll: reg(HandleFlushAll, 0, 0),
		resp.CmdAuth:     reg(HandleAuth, 1, 1), // mutates client auth state

		// String counter commands — all mutate.
		resp.CmdIncr:        reg(HandleIncr, 1, 1),
		resp.CmdDecr:        reg(HandleDecr, 1, 1),
		resp.CmdIncrBy:      reg(HandleIncrBy, 2, 2),
		resp.CmdDecrBy:      reg(HandleDecrBy, 2, 2),
		resp.CmdIncrByFloat: reg(HandleIncrByFloat, 2, 2),
		resp.CmdAppend:      reg(HandleAppend, 2, 2),
		resp.CmdStrlen:      regRO(HandleStrlen, 1, 1),

		// Multi-key commands
		resp.CmdMGet: regRO(HandleMget, 1, -1),
		resp.CmdMSet: reg(HandleMset, 2, -1),

		// Key management commands
		resp.CmdType:      regRO(HandleType, 1, 1),
		resp.CmdRename:    reg(HandleRename, 2, 2),
		resp.CmdRenameNX:  reg(HandleRenameNX, 2, 2),
		resp.CmdKeys:      regRO(HandleKeys, 1, 1),
		resp.CmdScan:      regRO(HandleScan, 1, -1),
		resp.CmdRandomKey: regRO(HandleRandomKey, 0, 0),

		// Watch commands — WATCH mutates the per-client watcher state in
		// WatchManager (registers interest); not read-only despite the
		// name. UNWATCH same.
		resp.CmdWatch:   reg(HandleWatch, 1, -1),
		resp.CmdUnwatch: reg(HandleUnwatch, 0, 0),

		// Key introspection — pure read.
		resp.CmdObject: regRO(HandleObject, 1, 2),
	}
}
