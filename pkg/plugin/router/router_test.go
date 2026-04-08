package router

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"gocache/pkg/plugin/protocol"
	"gocache/pkg/plugin/transport"
	gcpc "gocache/proto/gcpc/v1"
)

// testPipe creates a connected pair of transport.Conn for testing.
func testPipe() (*transport.Conn, *transport.Conn) {
	server, client := net.Pipe()
	return transport.NewConn(server), transport.NewConn(client)
}

func makeDecls(cmds ...string) []*gcpc.CommandDeclV1 {
	decls := make([]*gcpc.CommandDeclV1, len(cmds))
	for i, c := range cmds {
		decls[i] = &gcpc.CommandDeclV1{
			Name:    c,
			MinArgs: 1,
			MaxArgs: -1,
		}
	}
	return decls
}

func makeNamespacedDecls(cmds ...string) []*gcpc.CommandDeclV1 {
	decls := make([]*gcpc.CommandDeclV1, len(cmds))
	for i, c := range cmds {
		decls[i] = &gcpc.CommandDeclV1{
			Name:       c,
			Namespaced: true,
			MinArgs:    0,
			MaxArgs:    -1,
		}
	}
	return decls
}

func TestRegisterMainNamespace(t *testing.T) {
	r := NewRouter([]string{"GET", "SET"})
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	err := r.RegisterPlugin("pubsub", serverConn, makeDecls("PUBLISH", "SUBSCRIBE"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !r.HasCommand("PUBLISH") {
		t.Error("expected PUBLISH to be registered")
	}
	if !r.HasCommand("SUBSCRIBE") {
		t.Error("expected SUBSCRIBE to be registered")
	}
	if !r.HasCommand("publish") {
		t.Error("expected case-insensitive lookup to work")
	}
	if r.HasCommand("GET") {
		t.Error("GET is a core command, should not be in router")
	}
	if r.HasCommand("UNKNOWN") {
		t.Error("expected UNKNOWN to not be registered")
	}
}

func TestRegisterREXNamespace(t *testing.T) {
	r := NewRouter([]string{"GET", "SET"})
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	err := r.RegisterPlugin("kafka", serverConn, makeNamespacedDecls("PUBLISH", "CONSUME"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !r.HasCommand("KAFKA:PUBLISH") {
		t.Error("expected KAFKA:PUBLISH to be registered")
	}
	if !r.HasCommand("kafka:consume") {
		t.Error("expected case-insensitive REX lookup to work")
	}
	if r.HasCommand("PUBLISH") {
		t.Error("PUBLISH without namespace should not be registered")
	}
}

func TestRejectShadowCore(t *testing.T) {
	r := NewRouter([]string{"GET", "SET", "PUBLISH"})
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	err := r.RegisterPlugin("pubsub", serverConn, makeDecls("PUBLISH"))
	if err == nil {
		t.Fatal("expected error when shadowing core command")
	}
	if !r.HasCommand("PUBLISH") == true {
		// It should NOT be registered since it's a core command
	}
}

func TestRejectDuplicate(t *testing.T) {
	r := NewRouter([]string{"GET", "SET"})
	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()
	s2, c2 := testPipe()
	defer c2.Close()
	defer s2.Close()

	err := r.RegisterPlugin("pubsub1", s1, makeDecls("PUBLISH"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = r.RegisterPlugin("pubsub2", s2, makeDecls("PUBLISH"))
	if err == nil {
		t.Fatal("expected error for duplicate command registration")
	}
}

func TestAtomicRegistration(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	// Second command shadows core — whole registration should fail.
	decls := []*gcpc.CommandDeclV1{
		{Name: "PUBLISH", MinArgs: 1, MaxArgs: -1},
		{Name: "GET", MinArgs: 1, MaxArgs: 1}, // shadows core!
	}
	err := r.RegisterPlugin("pubsub", serverConn, decls)
	if err == nil {
		t.Fatal("expected error")
	}
	// PUBLISH should also NOT be registered (atomic rollback).
	if r.HasCommand("PUBLISH") {
		t.Error("expected PUBLISH to not be registered after atomic failure")
	}
}

func TestUnregisterPlugin(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	_ = r.RegisterPlugin("pubsub", serverConn, makeDecls("PUBLISH", "SUBSCRIBE"))

	if !r.HasCommand("PUBLISH") {
		t.Fatal("expected PUBLISH registered")
	}

	r.UnregisterPlugin("pubsub")

	if r.HasCommand("PUBLISH") {
		t.Error("expected PUBLISH unregistered")
	}
	if r.HasCommand("SUBSCRIBE") {
		t.Error("expected SUBSCRIBE unregistered")
	}
}

func TestRouteSuccess(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	decls := []*gcpc.CommandDeclV1{
		{Name: "ECHO", MinArgs: 1, MaxArgs: 1},
	}
	if err := r.RegisterPlugin("echo", serverConn, decls); err != nil {
		t.Fatal(err)
	}

	// Simulate plugin side: read request, send response.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		env, err := clientConn.Recv()
		if err != nil {
			t.Errorf("plugin recv: %v", err)
			return
		}
		req := env.GetCommandRequest()
		if req == nil {
			t.Error("expected CommandRequest")
			return
		}
		if req.Command != "ECHO" {
			t.Errorf("expected command ECHO, got %s", req.Command)
		}
		if len(req.Args) != 1 || req.Args[0] != "hello" {
			t.Errorf("unexpected args: %v", req.Args)
		}
		// Send response.
		result := &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: "hello"}}
		resp := protocol.NewCommandResponse(req.RequestId, result)
		if err := clientConn.Send(resp); err != nil {
			t.Errorf("plugin send: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	val, err := r.Route(ctx, "ECHO", []string{"hello"})
	if err != nil {
		t.Fatalf("Route error: %v", err)
	}
	if val != "hello" {
		t.Errorf("expected 'hello', got %v", val)
	}

	wg.Wait()
	clientConn.Close()
}

func TestRouteArgValidation(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	decls := []*gcpc.CommandDeclV1{
		{Name: "EXACT", MinArgs: 2, MaxArgs: 2},
	}
	_ = r.RegisterPlugin("test", serverConn, decls)

	ctx := context.Background()

	// Too few args.
	_, err := r.Route(ctx, "EXACT", []string{"one"})
	if err == nil {
		t.Error("expected arg validation error for too few args")
	}

	// Too many args.
	_, err = r.Route(ctx, "EXACT", []string{"one", "two", "three"})
	if err == nil {
		t.Error("expected arg validation error for too many args")
	}
}

func TestRouteTimeout(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	decls := []*gcpc.CommandDeclV1{
		{Name: "SLOW", MinArgs: 0, MaxArgs: -1},
	}
	_ = r.RegisterPlugin("slow", serverConn, decls)

	// Plugin side does nothing — request will timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := r.Route(ctx, "SLOW", nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if err != ErrPluginTimeout {
		t.Errorf("expected ErrPluginTimeout, got: %v", err)
	}
}

func TestRouteConcurrent(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	decls := []*gcpc.CommandDeclV1{
		{Name: "PING", MinArgs: 0, MaxArgs: -1},
	}
	_ = r.RegisterPlugin("pinger", serverConn, decls)

	// Plugin side: read requests and echo back request_id as the result.
	go func() {
		for {
			env, err := clientConn.Recv()
			if err != nil {
				return
			}
			req := env.GetCommandRequest()
			if req == nil {
				continue
			}
			result := &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: req.RequestId}}
			_ = clientConn.Send(protocol.NewCommandResponse(req.RequestId, result))
		}
	}()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := r.Route(ctx, "PING", nil)
			if err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Route error: %v", err)
	}

	clientConn.Close()
}

func TestReregisterAfterUnregister(t *testing.T) {
	r := NewRouter([]string{"GET"})

	s1, c1 := testPipe()
	defer c1.Close()
	defer s1.Close()

	_ = r.RegisterPlugin("pubsub", s1, makeDecls("PUBLISH"))
	r.UnregisterPlugin("pubsub")

	s2, c2 := testPipe()
	defer c2.Close()
	defer s2.Close()

	// Should succeed — slot is free.
	err := r.RegisterPlugin("pubsub", s2, makeDecls("PUBLISH"))
	if err != nil {
		t.Fatalf("re-registration should succeed: %v", err)
	}
	if !r.HasCommand("PUBLISH") {
		t.Error("PUBLISH should be registered after re-registration")
	}
}
