package handler

import (
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// Handlers returns all RESP command handlers keyed by command name.
func Handlers() map[string]command.Handler {
	return map[string]command.Handler{
		// Basic commands
		resp.CmdSet:     HandleSet,
		resp.CmdGet:     HandleGet,
		resp.CmdDelete:  HandleDelete,
		resp.CmdExists:  HandleExists,
		resp.CmdExpire:  HandleExpire,
		resp.CmdPExpire: HandlePexpire,
		resp.CmdTTL:     HandleTtl,
		resp.CmdPTTL:    HandlePttl,
		resp.CmdSetNX:   HandleSetnx,

		// List commands
		resp.CmdLPush:  HandleLpush,
		resp.CmdRPush:  HandleRpush,
		resp.CmdLPop:   HandleLpop,
		resp.CmdRPop:   HandleRpop,
		resp.CmdLLen:   HandleLlen,
		resp.CmdLRange: HandleLrange,
		resp.CmdBLPop:  HandleBlpop,
		resp.CmdBRPop:  HandleBrpop,

		// Hash commands
		resp.CmdHSet:    HandleHset,
		resp.CmdHGet:    HandleHget,
		resp.CmdHDel:    HandleHdel,
		resp.CmdHExists: HandleHexists,
		resp.CmdHGetAll: HandleHgetall,
		resp.CmdHKeys:   HandleHkeys,
		resp.CmdHVals:   HandleHvals,
		resp.CmdHLen:    HandleHlen,

		// Set commands
		resp.CmdSAdd:      HandleSadd,
		resp.CmdSRem:      HandleSrem,
		resp.CmdSMembers:  HandleSmembers,
		resp.CmdSIsMember: HandleSismember,
		resp.CmdSCard:     HandleScard,
		resp.CmdSPop:      HandleSpop,
		resp.CmdSInter:    HandleSinter,
		resp.CmdSUnion:    HandleSunion,
		resp.CmdSDiff:     HandleSdiff,

		// Sorted Set commands
		resp.CmdZAdd:   HandleZadd,
		resp.CmdZRem:   HandleZrem,
		resp.CmdZScore: HandleZscore,
		resp.CmdZCard:  HandleZcard,
		resp.CmdZRange: HandleZrange,
		resp.CmdZRank:  HandleZrank,
		resp.CmdZCount: HandleZcount,

		// Transaction commands
		resp.CmdMulti:   HandleMulti,
		resp.CmdDiscard: HandleDiscard,
		resp.CmdExec:    HandleExec,

		// Persistence commands
		resp.CmdSnapshot:     HandleSnapshot,
		resp.CmdLoadSnapshot: HandleLoadSnapshot,

		// Server commands
		resp.CmdDBSize:   HandleDbsize,
		resp.CmdInfo:     HandleInfo,
		resp.CmdHello:    HandleHello,
		resp.CmdPing:     HandlePing,
		resp.CmdEcho:     HandleEcho,
		resp.CmdSelect:   HandleSelect,
		resp.CmdFlushDB:  HandleFlushDB,
		resp.CmdFlushAll: HandleFlushAll,
		resp.CmdAuth:     HandleAuth,

		// String counter commands
		resp.CmdIncr:        HandleIncr,
		resp.CmdDecr:        HandleDecr,
		resp.CmdIncrBy:      HandleIncrBy,
		resp.CmdDecrBy:      HandleDecrBy,
		resp.CmdIncrByFloat: HandleIncrByFloat,
		resp.CmdAppend:      HandleAppend,
		resp.CmdStrlen:      HandleStrlen,

		// Multi-key commands
		resp.CmdMGet: HandleMget,
		resp.CmdMSet: HandleMset,

		// Key management commands
		resp.CmdType:      HandleType,
		resp.CmdRename:    HandleRename,
		resp.CmdRenameNX:  HandleRenameNX,
		resp.CmdKeys:      HandleKeys,
		resp.CmdScan:      HandleScan,
		resp.CmdRandomKey: HandleRandomKey,

		// Watch commands
		resp.CmdWatch:   HandleWatch,
		resp.CmdUnwatch: HandleUnwatch,

		// Key introspection
		resp.CmdObject: HandleObject,
	}
}
