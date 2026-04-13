package pluginsdk

import (
	"context"
	"net"
	"testing"
	"time"

	"gocache/pkg/plugin/protocol"
	"gocache/pkg/plugin/transport"
	gcpc "gocache/proto/gcpc/v1"
)

func TestSession_QueryServer(t *testing.T) {
	serverRaw, clientRaw := net.Pipe()
	serverConn := transport.NewConn(serverRaw)
	clientConn := transport.NewConn(clientRaw)
	defer serverConn.Close()
	defer clientConn.Close()

	session := newSession(clientConn)

	// Simulate server side: read query, respond.
	go func() {
		env, err := serverConn.Recv()
		if err != nil {
			t.Errorf("server recv: %v", err)
			return
		}
		query := env.GetServerQuery()
		if query == nil {
			t.Error("expected ServerQueryV1")
			return
		}
		if query.Topic != "health" {
			t.Errorf("expected topic 'health', got %q", query.Topic)
		}
		resp := protocol.NewServerQueryResponse(query.RequestId,
			map[string]string{"status": "ok", "uptime_ns": "1000000"},
			"")
		if err := serverConn.Send(resp); err != nil {
			t.Errorf("server send: %v", err)
		}
	}()

	// Client side needs to receive and dispatch in a goroutine (simulating Run's recv loop).
	go func() {
		env, err := clientConn.Recv()
		if err != nil {
			return
		}
		if resp := env.GetServerQueryResponse(); resp != nil {
			session.dispatch(resp)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, err := session.QueryServer(ctx, "health")
	if err != nil {
		t.Fatalf("QueryServer error: %v", err)
	}
	if data["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", data["status"])
	}
}

func TestSession_QueryServer_Error(t *testing.T) {
	serverRaw, clientRaw := net.Pipe()
	serverConn := transport.NewConn(serverRaw)
	clientConn := transport.NewConn(clientRaw)
	defer serverConn.Close()
	defer clientConn.Close()

	session := newSession(clientConn)

	go func() {
		env, _ := serverConn.Recv()
		query := env.GetServerQuery()
		resp := protocol.NewServerQueryResponse(query.RequestId, nil, "permission denied: missing scope \"server:query:health\"")
		_ = serverConn.Send(resp)
	}()

	go func() {
		env, err := clientConn.Recv()
		if err != nil {
			return
		}
		if resp := env.GetServerQueryResponse(); resp != nil {
			session.dispatch(resp)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := session.QueryServer(ctx, "health")
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "permission denied: missing scope \"server:query:health\"" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSession_QueryServer_Timeout(t *testing.T) {
	serverRaw, clientRaw := net.Pipe()
	clientConn := transport.NewConn(clientRaw)
	defer clientConn.Close()

	session := newSession(clientConn)

	// Server side: read the query but never respond.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := serverRaw.Read(buf); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := session.QueryServer(ctx, "health")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	serverRaw.Close()
}

func TestSession_Dispatch_NoWaiter(t *testing.T) {
	// Dispatching a response with no waiter should not panic.
	_, clientRaw := net.Pipe()
	clientConn := transport.NewConn(clientRaw)
	defer clientConn.Close()

	session := newSession(clientConn)
	session.dispatch(&gcpc.ServerQueryResponseV1{
		RequestId: "unknown-id",
		Data:      map[string]string{"status": "ok"},
	})
	// No panic = pass.
}
