package server

import (
	"gocache/pkg/resp"
	"testing"
)

func TestIT_ListOperations(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertInt(t, sendCommand(t, conn, "LPUSH", "list", "b"), 1)
	assertInt(t, sendCommand(t, conn, "LPUSH", "list", "a"), 2)
	assertInt(t, sendCommand(t, conn, "RPUSH", "list", "c"), 3)
	assertInt(t, sendCommand(t, conn, "LLEN", "list"), 3)

	v := sendCommand(t, conn, "LRANGE", "list", "0", "-1")
	if len(v.Array) != 3 {
		t.Fatalf("LRANGE: expected 3, got %d", len(v.Array))
	}
	assertBulk(t, v.Array[0], "a")
	assertBulk(t, v.Array[1], "b")
	assertBulk(t, v.Array[2], "c")

	assertBulk(t, sendCommand(t, conn, "LPOP", "list"), "a")
	assertBulk(t, sendCommand(t, conn, "RPOP", "list"), "c")
	assertInt(t, sendCommand(t, conn, "LLEN", "list"), 1)
}

func TestIT_HashOperations(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertInt(t, sendCommand(t, conn, "HSET", "h", "name", "gocache"), 1)
	assertInt(t, sendCommand(t, conn, "HSET", "h", "ver", "0.1"), 1)
	assertBulk(t, sendCommand(t, conn, "HGET", "h", "name"), "gocache")
	assertInt(t, sendCommand(t, conn, "HLEN", "h"), 2)
	assertInt(t, sendCommand(t, conn, "HEXISTS", "h", "name"), 1)
	assertInt(t, sendCommand(t, conn, "HEXISTS", "h", "missing"), 0)
	assertInt(t, sendCommand(t, conn, "HDEL", "h", "ver"), 1)
	assertInt(t, sendCommand(t, conn, "HLEN", "h"), 1)

	// HKEYS / HVALS
	v := sendCommand(t, conn, "HKEYS", "h")
	if len(v.Array) != 1 {
		t.Fatalf("HKEYS: expected 1, got %d", len(v.Array))
	}
	assertBulk(t, v.Array[0], "name")
}

func TestIT_SetOperations(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertInt(t, sendCommand(t, conn, "SADD", "s1", "a", "b", "c"), 3)
	assertInt(t, sendCommand(t, conn, "SCARD", "s1"), 3)
	assertInt(t, sendCommand(t, conn, "SISMEMBER", "s1", "a"), 1)
	assertInt(t, sendCommand(t, conn, "SISMEMBER", "s1", "z"), 0)
	assertInt(t, sendCommand(t, conn, "SREM", "s1", "c"), 1)
	assertInt(t, sendCommand(t, conn, "SCARD", "s1"), 2)
}

func TestIT_SetIntersectionUnionDiff(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	sendCommand(t, conn, "SADD", "sa", "a", "b", "c")
	sendCommand(t, conn, "SADD", "sb", "b", "c", "d")

	// SINTER: {b, c}
	inter := sendCommand(t, conn, "SINTER", "sa", "sb")
	if inter.Type != resp.Array || len(inter.Array) != 2 {
		t.Fatalf("SINTER: expected 2, got %+v", inter)
	}

	// SUNION: {a, b, c, d}
	union := sendCommand(t, conn, "SUNION", "sa", "sb")
	if union.Type != resp.Array || len(union.Array) != 4 {
		t.Fatalf("SUNION: expected 4, got %d", len(union.Array))
	}

	// SDIFF sa sb: {a}
	diff := sendCommand(t, conn, "SDIFF", "sa", "sb")
	if diff.Type != resp.Array || len(diff.Array) != 1 {
		t.Fatalf("SDIFF: expected 1, got %d", len(diff.Array))
	}
	assertBulk(t, diff.Array[0], "a")
}

func TestIT_SortedSetOperations(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertInt(t, sendCommand(t, conn, "ZADD", "zs", "1", "a"), 1)
	assertInt(t, sendCommand(t, conn, "ZADD", "zs", "2", "b"), 1)
	assertInt(t, sendCommand(t, conn, "ZADD", "zs", "3", "c"), 1)
	assertInt(t, sendCommand(t, conn, "ZCARD", "zs"), 3)
	assertBulk(t, sendCommand(t, conn, "ZSCORE", "zs", "b"), "2")
	assertInt(t, sendCommand(t, conn, "ZRANK", "zs", "a"), 0)
	assertInt(t, sendCommand(t, conn, "ZCOUNT", "zs", "1", "2"), 2)
	assertInt(t, sendCommand(t, conn, "ZREM", "zs", "c"), 1)
	assertInt(t, sendCommand(t, conn, "ZCARD", "zs"), 2)
}
