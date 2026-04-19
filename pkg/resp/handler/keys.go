package handler

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"gocache/pkg/cache"
	"gocache/pkg/command"
	"gocache/pkg/resp"
)

const (
	// defaultScanCount is the page size used by SCAN when COUNT is omitted.
	defaultScanCount = 10
	// embstrMaxLen is the Redis-compatible boundary between "embstr" and
	// "raw" string encodings (strings <= 44 bytes report as embstr).
	embstrMaxLen = 44
)

// HandleType implements TYPE key.
func HandleType(cmdCtx *command.Context) command.Result {
	key := cmdCtx.Args[0]
	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(key)
		if !found {
			return resp.Value{Type: resp.SimpleString, Str: "none"}
		}
		_, state := cmdCtx.Cache.TTLInternal(key)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(key)
			return resp.Value{Type: resp.SimpleString, Str: "none"}
		}
		var typeName string
		switch entry.ValueType {
		case cache.ObjTypeBytes:
			typeName = "string"
		case cache.ObjTypeList:
			typeName = "list"
		case cache.ObjTypeHash:
			typeName = "hash"
		case cache.ObjTypeSet:
			typeName = "set"
		case cache.ObjTypeSortedSet:
			typeName = "zset"
		default:
			typeName = "none"
		}
		return resp.Value{Type: resp.SimpleString, Str: typeName}
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleRename implements RENAME key newkey.
func HandleRename(cmdCtx *command.Context) command.Result {
	src := cmdCtx.Args[0]
	dst := cmdCtx.Args[1]
	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(src)
		if !found {
			return resp.MarshalError("ERR no such key")
		}
		_, state := cmdCtx.Cache.TTLInternal(src)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(src)
			return resp.MarshalError("ERR no such key")
		}
		ttl := cmdCtx.Cache.RawTTL(src)

		// Determine absolute expiration for the destination.
		var expiration int64
		if ttl > 0 {
			expiration = ttl
		}

		cmdCtx.Cache.RawDelete(dst)
		cmdCtx.Cache.RawDelete(src)
		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), dst, entry.Value, expiration); err != nil {
			return err
		}
		return "OK"
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleRenameNX implements RENAMENX key newkey.
func HandleRenameNX(cmdCtx *command.Context) command.Result {
	src := cmdCtx.Args[0]
	dst := cmdCtx.Args[1]
	executeFn := func() interface{} {
		entry, found := cmdCtx.Cache.RawGet(src)
		if !found {
			return resp.MarshalError("ERR no such key")
		}
		_, state := cmdCtx.Cache.TTLInternal(src)
		if state == cache.ValueExpired {
			cmdCtx.Cache.RawDelete(src)
			return resp.MarshalError("ERR no such key")
		}

		// Check if destination already exists.
		_, dstFound := cmdCtx.Cache.RawGet(dst)
		if dstFound {
			_, dstState := cmdCtx.Cache.TTLInternal(dst)
			if dstState != cache.ValueExpired {
				return 0
			}
			// Destination is expired — treat as absent.
			cmdCtx.Cache.RawDelete(dst)
		}

		ttl := cmdCtx.Cache.RawTTL(src)
		var expiration int64
		if ttl > 0 {
			expiration = ttl
		}

		cmdCtx.Cache.RawDelete(src)
		if err := cmdCtx.Cache.RawSet(cmdCtx.Context(), dst, entry.Value, expiration); err != nil {
			return err
		}
		return 1
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleKeys implements KEYS pattern.
func HandleKeys(cmdCtx *command.Context) command.Result {
	pattern := cmdCtx.Args[0]
	executeFn := func() interface{} {
		var keys []string
		now := time.Now().UnixNano()
		cmdCtx.Cache.Range(func(key string, _ *cache.Entry, expiration int64) bool {
			if expiration > 0 && expiration <= now {
				return true // skip expired
			}
			matched, err := path.Match(pattern, key)
			if err != nil {
				return true // skip on bad pattern
			}
			if matched {
				keys = append(keys, key)
			}
			return true
		})
		if keys == nil {
			keys = []string{}
		}
		return keys
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleScan implements SCAN cursor [MATCH pattern] [COUNT count].
func HandleScan(cmdCtx *command.Context) command.Result {
	cursorStr := cmdCtx.Args[0]
	cursor, err := strconv.Atoi(cursorStr)
	if err != nil || cursor < 0 {
		return command.Result{Value: resp.MarshalError("ERR value is not an integer or out of range")}
	}

	// Parse optional MATCH and COUNT arguments.
	pattern := ""
	count := defaultScanCount
	args := cmdCtx.Args[1:]
	for i := 0; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "MATCH":
			if i+1 < len(args) {
				pattern = args[i+1]
				i++
			}
		case "COUNT":
			if i+1 < len(args) {
				n, parseErr := strconv.Atoi(args[i+1])
				if parseErr == nil && n > 0 {
					count = n
				}
				i++
			}
		}
	}

	executeFn := func() interface{} {
		now := time.Now().UnixNano()

		// Collect all non-expired keys.
		var allKeys []string
		cmdCtx.Cache.Range(func(key string, _ *cache.Entry, expiration int64) bool {
			if expiration > 0 && expiration <= now {
				return true
			}
			allKeys = append(allKeys, key)
			return true
		})
		sort.Strings(allKeys)

		// Apply MATCH filter.
		filtered := allKeys
		if pattern != "" {
			filtered = allKeys[:0]
			for _, k := range allKeys {
				matched, matchErr := path.Match(pattern, k)
				if matchErr == nil && matched {
					filtered = append(filtered, k)
				}
			}
		}

		total := len(filtered)
		if cursor >= total {
			// Past end: return empty with cursor 0.
			return []interface{}{"0", []string{}}
		}

		end := cursor + count
		if end > total {
			end = total
		}
		page := filtered[cursor:end]

		nextCursor := end
		if nextCursor >= total {
			nextCursor = 0
		}

		return []interface{}{strconv.Itoa(nextCursor), page}
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleRandomKey implements RANDOMKEY.
func HandleRandomKey(cmdCtx *command.Context) command.Result {
	executeFn := func() interface{} {
		now := time.Now().UnixNano()
		var found string
		cmdCtx.Cache.Range(func(key string, _ *cache.Entry, expiration int64) bool {
			if expiration > 0 && expiration <= now {
				return true
			}
			found = key
			return false // stop on first non-expired key
		})
		if found == "" {
			return nil
		}
		return found
	}
	return command.Dispatch(cmdCtx, executeFn)
}

// HandleObject implements OBJECT subcommand [key].
func HandleObject(cmdCtx *command.Context) command.Result {
	sub := strings.ToUpper(cmdCtx.Args[0])
	switch sub {
	case "ENCODING":
		if len(cmdCtx.Args) < 2 {
			return command.Result{Value: resp.ErrArgs("object")}
		}
		key := cmdCtx.Args[1]
		return command.Dispatch(cmdCtx, func() interface{} {
			entry, found := cmdCtx.Cache.RawGet(key)
			if !found {
				return nil
			}
			_, state := cmdCtx.Cache.TTLInternal(key)
			if state == cache.ValueExpired {
				cmdCtx.Cache.RawDelete(key)
				return nil
			}
			switch entry.ValueType {
			case cache.ObjTypeBytes:
				s, _ := entry.Value.(string)
				if len(s) <= embstrMaxLen {
					return "embstr"
				}
				return "raw"
			case cache.ObjTypeList:
				return "linkedlist"
			case cache.ObjTypeHash:
				return "hashtable"
			case cache.ObjTypeSet:
				return "hashtable"
			case cache.ObjTypeSortedSet:
				return "skiplist"
			default:
				return "raw"
			}
		})
	case "IDLETIME":
		if len(cmdCtx.Args) < 2 {
			return command.Result{Value: resp.ErrArgs("object")}
		}
		key := cmdCtx.Args[1]
		return command.Dispatch(cmdCtx, func() interface{} {
			entry, found := cmdCtx.Cache.RawGet(key)
			if !found {
				return nil
			}
			_, state := cmdCtx.Cache.TTLInternal(key)
			if state == cache.ValueExpired {
				cmdCtx.Cache.RawDelete(key)
				return nil
			}
			idle := int(time.Since(entry.LastAccessed).Seconds())
			return idle
		})
	case "HELP":
		return command.Result{Value: []string{
			"OBJECT ENCODING <key> - Return the encoding of the object stored at <key>.",
			"OBJECT IDLETIME <key> - Return the idle time of the object stored at <key>.",
			"OBJECT HELP - Return help about the OBJECT command.",
		}}
	default:
		return command.Result{Value: resp.MarshalError(fmt.Sprintf("ERR unknown subcommand '%s'", cmdCtx.Args[0]))}
	}
}
