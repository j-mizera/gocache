package server

import (
	"testing"
	"time"
)

func TestIT_SetWithExpiry(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "ttl", "val", "EX", "1"))
	assertBulk(t, sendCommand(t, conn, "GET", "ttl"), "val")

	ttl := sendCommand(t, conn, "TTL", "ttl")
	if ttl.Integer < 0 || ttl.Integer > 1 {
		t.Errorf("TTL: expected 0-1, got %d", ttl.Integer)
	}

	time.Sleep(1100 * time.Millisecond)
	assertNil(t, sendCommand(t, conn, "GET", "ttl"))
}

func TestIT_PExpireAndPTTL(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "pk", "val"))
	assertInt(t, sendCommand(t, conn, "PEXPIRE", "pk", "500"), 1)

	pttl := sendCommand(t, conn, "PTTL", "pk")
	if pttl.Integer <= 0 || pttl.Integer > 500 {
		t.Errorf("PTTL: expected 1-500, got %d", pttl.Integer)
	}

	time.Sleep(600 * time.Millisecond)
	assertNil(t, sendCommand(t, conn, "GET", "pk"))
}

func TestIT_ExpireCommand(t *testing.T) {
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	assertOK(t, sendCommand(t, conn, "SET", "ek", "val"))
	assertInt(t, sendCommand(t, conn, "EXPIRE", "ek", "10"), 1)

	ttl := sendCommand(t, conn, "TTL", "ek")
	if ttl.Integer <= 0 || ttl.Integer > 10 {
		t.Errorf("TTL: expected 1-10, got %d", ttl.Integer)
	}

	// Non-existent key
	assertInt(t, sendCommand(t, conn, "TTL", "nokey"), -2)
}
