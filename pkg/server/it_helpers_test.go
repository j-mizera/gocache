package server

import (
	"net"
	"testing"

	"gocache/pkg/resp"
)

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return conn
}

func assertOK(t *testing.T, v resp.Value) {
	t.Helper()
	if v.Str != "OK" {
		t.Errorf("expected OK, got type=%c str=%q", v.Type, v.Str)
	}
}

func assertQueued(t *testing.T, v resp.Value) {
	t.Helper()
	if v.Str != "QUEUED" {
		t.Errorf("expected QUEUED, got type=%c str=%q", v.Type, v.Str)
	}
}

func assertBulk(t *testing.T, v resp.Value, want string) {
	t.Helper()
	if v.Str != want {
		t.Errorf("expected %q, got type=%c str=%q", want, v.Type, v.Str)
	}
}

func assertInt(t *testing.T, v resp.Value, want int) {
	t.Helper()
	if v.Type != resp.Integer || v.Integer != want {
		t.Errorf("expected integer %d, got type=%c int=%d str=%q", want, v.Type, v.Integer, v.Str)
	}
}

func assertNil(t *testing.T, v resp.Value) {
	t.Helper()
	// Nil in RESP2 can be BulkString with empty str or Null type.
	if v.Type == resp.Null {
		return
	}
	if v.Type == resp.BulkString && v.Str == "" {
		return
	}
	t.Errorf("expected nil, got type=%c str=%q int=%d", v.Type, v.Str, v.Integer)
}

func assertError(t *testing.T, v resp.Value, contains string) {
	t.Helper()
	if v.Type != resp.Error {
		t.Errorf("expected error containing %q, got type=%c str=%q", contains, v.Type, v.Str)
		return
	}
	if len(contains) > 0 {
		for i := 0; i <= len(v.Str)-len(contains); i++ {
			if v.Str[i:i+len(contains)] == contains {
				return
			}
		}
		t.Errorf("error %q does not contain %q", v.Str, contains)
	}
}
