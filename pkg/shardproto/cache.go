package shardproto

import "github.com/cespare/xxhash/v2"

// Cache is a sharded keyspace. Each key hashes to one shard; that shard's
// engine goroutine is the only writer and the only reader of the underlying
// map. Multi-key operations are intentionally unimplemented.
type Cache struct {
	shards []*Shard
	n      uint64
}

// NewCache constructs a cache with n shards. n must be > 0 and a power of
// two for the cheap mod-by-mask routing path. The caller pairs this with
// NewEngine(c) to start the per-shard goroutines.
func NewCache(n int) *Cache {
	if n <= 0 || n&(n-1) != 0 {
		panic("shardproto: shard count must be a positive power of two")
	}
	c := &Cache{shards: make([]*Shard, n), n: uint64(n)}
	for i := range c.shards {
		c.shards[i] = newShard()
	}
	return c
}

// shardIndex maps a key to its shard index. xxhash64 is well-distributed
// for arbitrary string keys; mask via (n-1) when n is a power of two.
func (c *Cache) shardIndex(key string) int {
	return int(xxhash.Sum64String(key) & (c.n - 1))
}

// ShardCount exposes N for benchmarks and metrics.
func (c *Cache) ShardCount() int { return int(c.n) }
