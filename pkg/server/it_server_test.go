package server

import (
	"gocache/pkg/resp"
	"testing"
)

func TestIT_Auth(t *testing.T) {
	_, addr := startTestServer(t, "pass123")
	conn := dial(t, addr)
	defer conn.Close()

	// Blocked before auth
	assertError(t, sendCommand(t, conn, "SET", "k", "v"), "NOAUTH")

	// Wrong password
	assertError(t, sendCommand(t, conn, "AUTH", "wrong"), "WRONGPASS")

	// Correct password
	assertOK(t, sendCommand(t, conn, "AUTH", "pass123"))
	assertOK(t, sendCommand(t, conn, "SET", "k", "v"))
	assertBulk(t, sendCommand(t, conn, "GET", "k"), "v")
}

func TestIT_PingEchoInfo(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	v := sendCommand(t, conn, "PING")
	if v.Str != "PONG" {
		t.Errorf("PING: expected PONG, got %q", v.Str)
	}

	assertBulk(t, sendCommand(t, conn, "PING", "hello"), "hello")
	assertBulk(t, sendCommand(t, conn, "ECHO", "test"), "test")

	info := sendCommand(t, conn, "INFO")
	if info.Type != resp.BulkString || len(info.Str) == 0 {
		t.Errorf("INFO: expected non-empty bulk string, got %+v", info)
	}
}

func TestIT_FlushDB(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "a", "1"))
	assertOK(t, sendCommand(t, conn, "SET", "b", "2"))
	assertOK(t, sendCommand(t, conn, "FLUSHDB"))
	assertNil(t, sendCommand(t, conn, "GET", "a"))
	assertNil(t, sendCommand(t, conn, "GET", "b"))
}

func TestIT_KeyManagement(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "old", "val"))
	assertOK(t, sendCommand(t, conn, "RENAME", "old", "new"))
	assertNil(t, sendCommand(t, conn, "GET", "old"))
	assertBulk(t, sendCommand(t, conn, "GET", "new"), "val")

	v := sendCommand(t, conn, "TYPE", "new")
	assertBulk(t, v, "string")

	dbsize := sendCommand(t, conn, "DBSIZE")
	if dbsize.Integer < 1 {
		t.Errorf("DBSIZE: expected >= 1, got %d", dbsize.Integer)
	}
}

func TestIT_WrongType(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "str", "val"))
	assertError(t, sendCommand(t, conn, "LPUSH", "str", "x"), "WRONGTYPE")
}

func TestIT_WrongArgCount(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertError(t, sendCommand(t, conn, "GET"), "ERR")
	assertError(t, sendCommand(t, conn, "SET", "onlykey"), "ERR")
}
