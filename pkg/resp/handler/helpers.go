package handler

import "gocache/pkg/cache"

// lazyExpire performs the "check TTL, delete if expired" pattern that almost
// every read-style handler needs after a RawGet. Must be called while
// holding the cache write lock (i.e. inside a command.Dispatch closure).
// Returns true if the key was expired and has been deleted; callers
// typically use that to treat the key as absent:
//
//	if lazyExpire(c, key) {
//	    return nil
//	}
//
// Callers that don't care about the return value can ignore it — the
// function is a no-op for keys that are missing or still live.
func lazyExpire(c *cache.Cache, key string) bool {
	_, state := c.TTLInternal(key)
	if state == cache.ValueExpired {
		c.RawDelete(key)
		return true
	}
	return false
}
