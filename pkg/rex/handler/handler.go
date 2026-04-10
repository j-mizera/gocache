package handler

import (
	"strings"

	"gocache/pkg/command"
	"gocache/pkg/resp"
	"gocache/pkg/rex"
)

// HandleRexMeta handles the REX.META command for connection-scoped metadata.
//
// Subcommands:
//
//	REX.META SET <key> <value>                -> +OK
//	REX.META MSET <key> <val> [<key> <val>..] -> +OK
//	REX.META GET <key>                        -> bulk string or nil
//	REX.META DEL <key>                        -> :1 or :0
//	REX.META LIST                             -> map (RESP3) or flat array (RESP2)
func HandleRexMeta(cmdCtx *command.Context) command.Result {
	sub := strings.ToUpper(cmdCtx.Args[0])

	switch sub {
	case "SET":
		if len(cmdCtx.Args) < 3 {
			return command.Result{Value: resp.ErrArgs("rex.meta set")}
		}
		key := cmdCtx.Args[1]
		value := strings.Join(cmdCtx.Args[2:], " ")
		store := ensureRexStore(cmdCtx)
		if err := store.Set(key, value); err != nil {
			return command.Result{Value: resp.MarshalError("ERR " + err.Error())}
		}
		return command.Result{Value: "OK"}

	case "MSET":
		if len(cmdCtx.Args) < 3 || (len(cmdCtx.Args)-1)%2 != 0 {
			return command.Result{Value: resp.MarshalError("ERR wrong number of arguments for REX.META MSET")}
		}
		store := ensureRexStore(cmdCtx)
		pairs := cmdCtx.Args[1:]
		for i := 0; i < len(pairs); i += 2 {
			if err := store.Set(pairs[i], pairs[i+1]); err != nil {
				return command.Result{Value: resp.MarshalError("ERR " + err.Error())}
			}
		}
		return command.Result{Value: "OK"}

	case "GET":
		if len(cmdCtx.Args) != 2 {
			return command.Result{Value: resp.ErrArgs("rex.meta get")}
		}
		if cmdCtx.Client.RexMeta == nil {
			return command.Result{Value: nil}
		}
		v, ok := cmdCtx.Client.RexMeta.Get(cmdCtx.Args[1])
		if !ok {
			return command.Result{Value: nil}
		}
		return command.Result{Value: v}

	case "DEL":
		if len(cmdCtx.Args) != 2 {
			return command.Result{Value: resp.ErrArgs("rex.meta del")}
		}
		if cmdCtx.Client.RexMeta == nil {
			return command.Result{Value: 0}
		}
		if cmdCtx.Client.RexMeta.Del(cmdCtx.Args[1]) {
			return command.Result{Value: 1}
		}
		return command.Result{Value: 0}

	case "LIST":
		if cmdCtx.Client.RexMeta == nil || cmdCtx.Client.RexMeta.Len() == 0 {
			return command.Result{Value: map[string]string{}}
		}
		return command.Result{Value: cmdCtx.Client.RexMeta.All()}

	default:
		return command.Result{Value: resp.MarshalError("ERR unknown REX.META subcommand '" + sub + "'")}
	}
}

func ensureRexStore(cmdCtx *command.Context) *rex.Store {
	if cmdCtx.Client.RexMeta == nil {
		cmdCtx.Client.RexMeta = rex.NewStore()
	}
	return cmdCtx.Client.RexMeta
}
