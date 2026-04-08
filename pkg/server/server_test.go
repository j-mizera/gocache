package server

import (
	"context"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/engine"
	"gocache/pkg/resp"
	"gocache/pkg/watch"
	"net"
	"testing"
	"time"
)

func startTestServer(t *testing.T, requirePass string) (*Server, string) {
	t.Helper()
	c := cache.New()
	e := engine.New(c)
	go e.Run()
	t.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()

	srv := New("127.0.0.1:0", c, e, "", requirePass, br, wm)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.listener = listener

	go srv.acceptConnections()
	t.Cleanup(func() { srv.Shutdown(2 * time.Second) })

	return srv, listener.Addr().String()
}

func sendCommand(t *testing.T, conn net.Conn, args ...string) resp.Value {
	t.Helper()
	w := resp.NewWriter(conn)
	vals := make([]resp.Value, len(args))
	for i, a := range args {
		vals[i] = resp.MarshalBulkString(a)
	}
	if err := w.Write(resp.ValueArray(vals...)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	r := resp.NewReader(conn)
	val, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return val
}

func TestServer_PingPong(t *testing.T) {
	_, addr := startTestServer(t, "")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	val := sendCommand(t, conn, "PING")
	if val.Str != "PONG" {
		t.Errorf("expected PONG, got %q", val.Str)
	}
}

func TestServer_SetGet(t *testing.T) {
	_, addr := startTestServer(t, "")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	val := sendCommand(t, conn, "SET", "foo", "bar")
	if val.Str != "OK" {
		t.Errorf("SET: expected OK, got %q", val.Str)
	}

	val = sendCommand(t, conn, "GET", "foo")
	if val.Str != "bar" {
		t.Errorf("GET: expected bar, got %q", val.Str)
	}
}

func TestServer_Quit(t *testing.T) {
	_, addr := startTestServer(t, "")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	val := sendCommand(t, conn, "QUIT")
	if val.Str != "OK" {
		t.Errorf("QUIT: expected OK, got %q", val.Str)
	}

	// Connection should be closed by server.
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected read error after QUIT")
	}
}

func TestServer_AuthGate(t *testing.T) {
	_, addr := startTestServer(t, "secret")

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Command before auth should be rejected.
	val := sendCommand(t, conn, "PING")
	if val.Type != resp.Error {
		t.Errorf("expected error before auth, got type %c: %q", val.Type, val.Str)
	}

	// Auth with correct password.
	val = sendCommand(t, conn, "AUTH", "secret")
	if val.Str != "OK" {
		t.Errorf("AUTH: expected OK, got %q", val.Str)
	}

	// Now commands should work.
	val = sendCommand(t, conn, "PING")
	if val.Str != "PONG" {
		t.Errorf("expected PONG after auth, got %q", val.Str)
	}
}

func TestServer_Shutdown(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	go e.Run()
	defer e.Stop()

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	srv := New("127.0.0.1:0", c, e, "", "", br, wm)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Give server time to start.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}
