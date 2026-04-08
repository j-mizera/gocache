package server

import "testing"

func TestIT_StringSetGet(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "key1", "hello"))
	assertBulk(t, sendCommand(t, conn, "GET", "key1"), "hello")

	// Overwrite
	assertOK(t, sendCommand(t, conn, "SET", "key1", "world"))
	assertBulk(t, sendCommand(t, conn, "GET", "key1"), "world")

	// Non-existent
	assertNil(t, sendCommand(t, conn, "GET", "nokey"))
}

func TestIT_Setnx(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertInt(t, sendCommand(t, conn, "SETNX", "nx", "first"), 1)
	assertInt(t, sendCommand(t, conn, "SETNX", "nx", "second"), 0)
	assertBulk(t, sendCommand(t, conn, "GET", "nx"), "first")
}

func TestIT_AppendStrlen(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "s", "hello"))
	assertInt(t, sendCommand(t, conn, "APPEND", "s", " world"), 11)
	assertInt(t, sendCommand(t, conn, "STRLEN", "s"), 11)
	assertBulk(t, sendCommand(t, conn, "GET", "s"), "hello world")
}

func TestIT_Counters(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "c", "10"))
	assertInt(t, sendCommand(t, conn, "INCR", "c"), 11)
	assertInt(t, sendCommand(t, conn, "DECR", "c"), 10)
	assertInt(t, sendCommand(t, conn, "INCRBY", "c", "5"), 15)
	assertInt(t, sendCommand(t, conn, "DECRBY", "c", "3"), 12)
	assertBulk(t, sendCommand(t, conn, "INCRBYFLOAT", "c", "1.5"), "13.5")
}

func TestIT_MultiKey(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "MSET", "a", "1", "b", "2", "c", "3"))

	v := sendCommand(t, conn, "MGET", "a", "b", "c", "missing")
	if len(v.Array) != 4 {
		t.Fatalf("MGET: expected 4 elements, got %d", len(v.Array))
	}
	assertBulk(t, v.Array[0], "1")
	assertBulk(t, v.Array[1], "2")
	assertBulk(t, v.Array[2], "3")
	assertNil(t, v.Array[3])
}

func TestIT_DelExists(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "k", "v"))
	assertInt(t, sendCommand(t, conn, "EXISTS", "k"), 1)
	assertInt(t, sendCommand(t, conn, "DEL", "k"), 1)
	assertInt(t, sendCommand(t, conn, "EXISTS", "k"), 0)
	assertNil(t, sendCommand(t, conn, "GET", "k"))
}
