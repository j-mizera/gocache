package server

import (
	"gocache/pkg/resp"
	"testing"
)

func TestIT_MultiExec(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "MULTI"))
	assertQueued(t, sendCommand(t, conn, "SET", "tx", "val"))
	assertQueued(t, sendCommand(t, conn, "GET", "tx"))

	v := sendCommand(t, conn, "EXEC")
	if v.Type != resp.Array || len(v.Array) != 2 {
		t.Fatalf("EXEC: expected 2-element array, got %+v", v)
	}
	if v.Array[0].Str != "OK" {
		t.Errorf("EXEC[0]: expected OK, got %q", v.Array[0].Str)
	}
	assertBulk(t, v.Array[1], "val")
}

func TestIT_Discard(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "dk", "before"))
	assertOK(t, sendCommand(t, conn, "MULTI"))
	assertQueued(t, sendCommand(t, conn, "SET", "dk", "after"))
	assertOK(t, sendCommand(t, conn, "DISCARD"))

	assertBulk(t, sendCommand(t, conn, "GET", "dk"), "before")
}

func TestIT_WatchAbort(t *testing.T) {
	_, addr := startTestServer(t, "")
	c1 := dial(t, addr)
	c2 := dial(t, addr)
	defer c1.Close()
	defer c2.Close()

	assertOK(t, sendCommand(t, c1, "SET", "wk", "original"))

	// c1: WATCH + MULTI + queue
	assertOK(t, sendCommand(t, c1, "WATCH", "wk"))
	assertOK(t, sendCommand(t, c1, "MULTI"))
	assertQueued(t, sendCommand(t, c1, "SET", "wk", "from-c1"))

	// c2: modify watched key
	assertOK(t, sendCommand(t, c2, "SET", "wk", "from-c2"))

	// c1: EXEC aborts (nil)
	assertNil(t, sendCommand(t, c1, "EXEC"))

	// Value is from c2
	assertBulk(t, sendCommand(t, c2, "GET", "wk"), "from-c2")
}

func TestIT_TransactionIsolation(t *testing.T) {
	_, addr := startTestServer(t, "")
	c1 := dial(t, addr)
	c2 := dial(t, addr)
	defer c1.Close()
	defer c2.Close()

	assertOK(t, sendCommand(t, c1, "SET", "iso", "old"))
	assertOK(t, sendCommand(t, c1, "MULTI"))
	assertQueued(t, sendCommand(t, c1, "SET", "iso", "new"))

	// c2 sees old value before c1 EXEC
	assertBulk(t, sendCommand(t, c2, "GET", "iso"), "old")

	sendCommand(t, c1, "EXEC")
	assertBulk(t, sendCommand(t, c2, "GET", "iso"), "new")
}
