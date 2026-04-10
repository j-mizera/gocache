package resp

const (
	CmdSet    = "SET"
	CmdGet    = "GET"
	CmdDelete = "DEL"
	CmdExists = "EXISTS"
	CmdExpire = "EXPIRE"
	CmdTTL    = "TTL"

	CmdLPush  = "LPUSH"
	CmdRPush  = "RPUSH"
	CmdLPop   = "LPOP"
	CmdRPop   = "RPOP"
	CmdLLen   = "LLEN"
	CmdLRange = "LRANGE"
	CmdBLPop  = "BLPOP"
	CmdBRPop  = "BRPOP"

	CmdHSet    = "HSET"
	CmdHGet    = "HGET"
	CmdHDel    = "HDEL"
	CmdHExists = "HEXISTS"
	CmdHGetAll = "HGETALL"
	CmdHKeys   = "HKEYS"
	CmdHVals   = "HVALS"
	CmdHLen    = "HLEN"

	CmdSAdd      = "SADD"
	CmdSRem      = "SREM"
	CmdSMembers  = "SMEMBERS"
	CmdSIsMember = "SISMEMBER"
	CmdSCard     = "SCARD"
	CmdSPop      = "SPOP"

	CmdZAdd   = "ZADD"
	CmdZRem   = "ZREM"
	CmdZScore = "ZSCORE"
	CmdZCard  = "ZCARD"
	CmdZRange = "ZRANGE"
	CmdZRank  = "ZRANK"
	CmdZCount = "ZCOUNT"

	CmdMulti   = "MULTI"
	CmdExec    = "EXEC"
	CmdDiscard = "DISCARD"

	CmdSnapshot     = "SNAPSHOT"
	CmdLoadSnapshot = "LOAD_SNAPSHOT"

	CmdDBSize = "DBSIZE"
	CmdInfo   = "INFO"
	CmdHello  = "HELLO"

	// Server/connection commands
	CmdPing     = "PING"
	CmdEcho     = "ECHO"
	CmdSelect   = "SELECT"
	CmdFlushDB  = "FLUSHDB"
	CmdFlushAll = "FLUSHALL"
	CmdAuth     = "AUTH"

	// String counter commands
	CmdIncr        = "INCR"
	CmdDecr        = "DECR"
	CmdIncrBy      = "INCRBY"
	CmdDecrBy      = "DECRBY"
	CmdIncrByFloat = "INCRBYFLOAT"
	CmdAppend      = "APPEND"
	CmdStrlen      = "STRLEN"

	// Multi-key commands
	CmdMGet = "MGET"
	CmdMSet = "MSET"

	// Key management commands
	CmdType      = "TYPE"
	CmdRename    = "RENAME"
	CmdRenameNX  = "RENAMENX"
	CmdKeys      = "KEYS"
	CmdScan      = "SCAN"
	CmdRandomKey = "RANDOMKEY"

	// SET variants and TTL commands
	CmdSetNX   = "SETNX"
	CmdPExpire = "PEXPIRE"
	CmdPTTL    = "PTTL"

	// Set operations
	CmdSInter = "SINTER"
	CmdSUnion = "SUNION"
	CmdSDiff  = "SDIFF"

	// Watch commands (optimistic locking)
	CmdWatch   = "WATCH"
	CmdUnwatch = "UNWATCH"

	// Key introspection
	CmdObject = "OBJECT"

	// REX metadata
	CmdMeta    = "META"     // Per-command metadata line (requires REXV negotiation)
	CmdRexMeta = "REX.META" // Connection-scoped metadata management
)
