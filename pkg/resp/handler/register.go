package handler

import (
	apicommand "gocache/api/command"
	"gocache/pkg/command"
	"gocache/commons/resp"
)

// reg builds a single-key Registration with the key at Args[0] (the
// default for the vast majority of commands). Use this for simple
// mutating handlers like SET, HSET, SADD, ZADD, INCR, etc.
func reg(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: apicommand.Spec{Min: min, Max: max}}
}

// regRO is reg with Spec.ReadOnly = true. Use only for handlers that
// strictly read from the cache and never mutate (no SET, no DEL, no
// expiration update, no LRU mutation, no WATCH dirty-bit propagation).
// Read-like commands with side effects (GETSET, GETDEL, GETEX, BLPOP,
// LPOP, SPOP, SRANDMEMBER-with-count-equal-to-cardinality) must use reg.
func regRO(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: apicommand.Spec{Min: min, Max: max, ReadOnly: true}}
}

// regKeyless builds a Registration for commands that do not address
// any cache key — client-state and server-meta commands like PING,
// AUTH, HELLO, MULTI, DISCARD, INFO, ECHO, SELECT, UNWATCH. The
// sharded engine runs these inline without dispatching through any
// shard's queue.
func regKeyless(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: apicommand.Spec{Min: min, Max: max, KeyArgIndex: -1}}
}

// regKeylessRO is regKeyless with ReadOnly = true. PING, ECHO, INFO.
func regKeylessRO(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: apicommand.Spec{Min: min, Max: max, ReadOnly: true, KeyArgIndex: -1}}
}

// regMulti marks commands that touch multiple keys or iterate the
// whole keyspace. The sharded engine routes these through cross-shard
// coordination (sorted-shard-ID lock acquisition) rather than
// dispatching to a single shard.
//
// Examples: DELETE-with-N-args, MGET, MSET, RENAME, KEYS, SCAN,
// SINTER, SUNION, SDIFF, BLPOP-with-N-keys, FLUSHDB, FLUSHALL,
// DBSIZE, RANDOMKEY, EXEC (whose queued commands can touch any
// shard), SNAPSHOT, LOADSNAPSHOT, WATCH, UNWATCH (per-shard
// watch.Manager updates).
func regMulti(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: apicommand.Spec{Min: min, Max: max, MultiKey: true}}
}

// regMultiRO is regMulti with ReadOnly = true.
func regMultiRO(h command.Handler, min, max int) command.Registration {
	return command.Registration{Handler: h, Spec: apicommand.Spec{Min: min, Max: max, ReadOnly: true, MultiKey: true}}
}

// Registrations returns all RESP command handlers with their argument specs.
//
// Spec.KeyArgIndex / Spec.MultiKey classification is used by the
// sharded engine to route commands. Today the production single-engine
// dispatcher ignores both; the metadata is in place so a sharded
// engine can route without per-handler reflection. See api/apicommand.Spec
// doc comments for classification rules.
func Registrations() map[string]command.Registration {
	return map[string]command.Registration{
		// String commands — single-key, key at Args[0].
		resp.CmdSet:     reg(HandleSet, 2, -1),
		resp.CmdGet:     regRO(HandleGet, 1, 1),
		resp.CmdDelete:  regMulti(HandleDelete, 1, -1), // multi-arg in this codebase
		resp.CmdExists:  regRO(HandleExists, 1, 1),    // single-arg only
		resp.CmdExpire:  reg(HandleExpire, 2, 2),
		resp.CmdPExpire: reg(HandlePexpire, 2, 2),
		resp.CmdTTL:     regRO(HandleTtl, 1, 1),
		resp.CmdPTTL:    regRO(HandlePttl, 1, 1),
		resp.CmdSetNX:   reg(HandleSetnx, 2, 2),

		// List commands — single-key.
		resp.CmdLPush:  reg(HandleLpush, 2, -1),
		resp.CmdRPush:  reg(HandleRpush, 2, -1),
		resp.CmdLPop:   reg(HandleLpop, 1, 1),
		resp.CmdRPop:   reg(HandleRpop, 1, 1),
		resp.CmdLLen:   regRO(HandleLlen, 1, 1),
		resp.CmdLRange: regRO(HandleLRange, 3, 3),
		// BLPOP/BRPOP take "key [key ...] timeout" — multi-key.
		resp.CmdBLPop: regMulti(HandleBlpop, 2, -1),
		resp.CmdBRPop: regMulti(HandleBrpop, 2, -1),

		// Hash commands — single-key.
		resp.CmdHSet:    reg(HandleHset, 3, -1),
		resp.CmdHGet:    regRO(HandleHget, 2, 2),
		resp.CmdHDel:    reg(HandleHdel, 2, -1),
		resp.CmdHExists: regRO(HandleHexists, 2, 2),
		resp.CmdHGetAll: regRO(HandleHgetall, 1, 1),
		resp.CmdHKeys:   regRO(HandleHkeys, 1, 1),
		resp.CmdHVals:   regRO(HandleHvals, 1, 1),
		resp.CmdHLen:    regRO(HandleHlen, 1, 1),

		// Set commands — single-key for SADD/SREM/etc.; multi-key for set-ops.
		resp.CmdSAdd:      reg(HandleSadd, 2, -1),
		resp.CmdSRem:      reg(HandleSrem, 2, -1),
		resp.CmdSMembers:  regRO(HandleSmembers, 1, 1),
		resp.CmdSIsMember: regRO(HandleSismember, 2, 2),
		resp.CmdSCard:     regRO(HandleScard, 1, 1),
		resp.CmdSPop:      reg(HandleSpop, 1, 1),
		// SINTER/SUNION/SDIFF take 1..N set keys — multi-key.
		resp.CmdSInter: regMultiRO(HandleSinter, 1, -1),
		resp.CmdSUnion: regMultiRO(HandleSunion, 1, -1),
		resp.CmdSDiff:  regMultiRO(HandleSdiff, 1, -1),

		// Sorted Set commands — single-key.
		resp.CmdZAdd:   reg(HandleZadd, 3, -1),
		resp.CmdZRem:   reg(HandleZrem, 2, -1),
		resp.CmdZScore: regRO(HandleZscore, 2, 2),
		resp.CmdZCard:  regRO(HandleZcard, 1, 1),
		resp.CmdZRange: regRO(HandleZrange, 3, 4),
		resp.CmdZRank:  regRO(HandleZrank, 2, 2),
		resp.CmdZCount: regRO(HandleZcount, 3, 3),

		// Transaction commands. MULTI/DISCARD only mutate per-client
		// transaction state — keyless. EXEC executes queued commands
		// that may touch any shard — multi-key for sharded routing.
		resp.CmdMulti:   regKeyless(HandleMulti, 0, 0),
		resp.CmdDiscard: regKeyless(HandleDiscard, 0, 0),
		resp.CmdExec:    regMulti(HandleExec, 0, 0),

		// Server / client-state commands.
		resp.CmdDBSize:   regMultiRO(HandleDBSize, 0, 0), // counts every key
		resp.CmdInfo:     regKeylessRO(HandleInfo, 0, 1),
		resp.CmdHello:    regKeyless(HandleHello, 1, -1), // mutates client proto-version state
		resp.CmdPing:     regKeylessRO(HandlePing, 0, 1),
		resp.CmdEcho:     regKeylessRO(HandleEcho, 1, 1),
		resp.CmdSelect:   regKeyless(HandleSelect, 1, 1),
		resp.CmdFlushDB:  regMulti(HandleFlushDB, 0, 0),  // clears every key
		resp.CmdFlushAll: regMulti(HandleFlushAll, 0, 0), // clears every key
		resp.CmdAuth:     regKeyless(HandleAuth, 1, 1),   // mutates client auth state

		// String counter commands — single-key.
		resp.CmdIncr:        reg(HandleIncr, 1, 1),
		resp.CmdDecr:        reg(HandleDecr, 1, 1),
		resp.CmdIncrBy:      reg(HandleIncrBy, 2, 2),
		resp.CmdDecrBy:      reg(HandleDecrBy, 2, 2),
		resp.CmdIncrByFloat: reg(HandleIncrByFloat, 2, 2),
		resp.CmdAppend:      reg(HandleAppend, 2, 2),
		resp.CmdStrlen:      regRO(HandleStrlen, 1, 1),

		// Multi-key string commands.
		resp.CmdMGet: regMultiRO(HandleMget, 1, -1),
		resp.CmdMSet: regMulti(HandleMset, 2, -1),

		// Key management.
		resp.CmdType:      regRO(HandleType, 1, 1),
		resp.CmdRename:    regMulti(HandleRename, 2, 2),   // src + dst
		resp.CmdRenameNX:  regMulti(HandleRenameNX, 2, 2), // src + dst
		resp.CmdKeys:      regMultiRO(HandleKeys, 1, 1),   // pattern over all keys
		resp.CmdScan:      regMultiRO(HandleScan, 1, -1),  // cursor over all keys
		resp.CmdRandomKey: regMultiRO(HandleRandomKey, 0, 0),

		// WATCH/UNWATCH coordinate watcher state across shards. WATCH
		// mutates the per-client watcher state in WatchManager
		// (registers interest); not read-only despite the name.
		resp.CmdWatch:   regMulti(HandleWatch, 1, -1),
		resp.CmdUnwatch: regKeyless(HandleUnwatch, 0, 0), // walks ctx.WatchedKeys, no input arg

		// Key introspection — pure read on a single key.
		resp.CmdObject: regRO(HandleObject, 1, 2),
	}
}
