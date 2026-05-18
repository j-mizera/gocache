package command

import "errors"

// Redis-compatible error sentinels returned by command handlers.
// These live in api/ so both the server core and command plugins can
// reference them without importing server-internal packages.
//
// Sentinels that flow through mapToResp's default branch get an "ERR "
// prefix automatically; messages here omit that prefix. Sentinels
// whose RESP encoding is non-standard (WRONGTYPE) include the full
// prefix in the message and are caught by dedicated branches.
var (
	ErrWrongType         = errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	ErrNotInteger        = errors.New("value is not an integer or out of range")
	ErrNotFloat          = errors.New("value is not a valid float")
	ErrInvalidExpireTime = errors.New("invalid expire time")
	ErrInvalidTimeout    = errors.New("timeout is not a float or out of range")
)
