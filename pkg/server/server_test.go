package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/engine"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/resp"
	"gocache/pkg/watch"
)

func startTestServer(t *testing.T, requirePass string) (*Server, string) {
	t.Helper()
	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	c.SetOnMutate(wm.NotifyMutation)
	c.SetOnMutateAll(wm.NotifyAll)

	srv := New("127.0.0.1:0", c, e, requirePass, br, wm)
	srv.SetTracker(serverOps.NewTracker())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.listener = listener

	go srv.acceptConnections(context.Background())
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
	defer e.Stop()

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	srv := New("127.0.0.1:0", c, e, "", br, wm)
	srv.SetTracker(serverOps.NewTracker())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Give server time to start.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

// captureListener wraps an underlying net.Listener and records each accepted
// connection so tests can inspect the server-side socket options after the
// server has processed connection setup.
type captureListener struct {
	inner net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (c *captureListener) Accept() (net.Conn, error) {
	conn, err := c.inner.Accept()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.conns = append(c.conns, conn)
	c.mu.Unlock()
	return conn, nil
}

func (c *captureListener) Close() error   { return c.inner.Close() }
func (c *captureListener) Addr() net.Addr { return c.inner.Addr() }

// TestServer_TCPNoDelay verifies handleConnection sets TCP_NODELAY on
// accepted connections so single-command-per-RTT clients don't pay Nagle's
// 40 ms delayed-ack stall. Closes #24.
func TestServer_TCPNoDelay(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	c.SetOnMutate(wm.NotifyMutation)
	c.SetOnMutateAll(wm.NotifyAll)

	srv := New("127.0.0.1:0", c, e, "", br, wm)
	srv.SetTracker(serverOps.NewTracker())

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cap := &captureListener{inner: inner}
	srv.listener = cap

	go srv.acceptConnections(context.Background())
	t.Cleanup(func() { _ = srv.Shutdown(2 * time.Second) })

	// Drive a real exchange so handleConnection's setup code (including
	// SetNoDelay) runs to completion before we inspect the socket.
	conn, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if v := sendCommand(t, conn, "PING"); v.Str != "PONG" {
		t.Fatalf("unexpected PING reply: %q", v.Str)
	}

	cap.mu.Lock()
	if len(cap.conns) == 0 {
		cap.mu.Unlock()
		t.Fatal("captureListener saw no accepted connections")
	}
	srvConn := cap.conns[0]
	cap.mu.Unlock()

	tcpConn, ok := srvConn.(*net.TCPConn)
	if !ok {
		t.Fatalf("server-side conn is not *net.TCPConn: %T", srvConn)
	}
	raw, err := tcpConn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}

	var noDelay int
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		noDelay, sockErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if sockErr != nil {
		t.Fatalf("getsockopt TCP_NODELAY: %v", sockErr)
	}
	if noDelay != 1 {
		t.Errorf("server-side TCP_NODELAY = %d, want 1 (Nagle disabled)", noDelay)
	}
}
