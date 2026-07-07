package router

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	gcpc "gocache/api/gcpc/v1"
	"gocache/commons/transport"
)

func BenchmarkPluginConnSendFireAndForget(b *testing.B) {
	env := &gcpc.EnvelopeV1{
		Version: gcpc.ProtocolVersion,
		Payload: &gcpc.EnvelopeV1_Event{Event: &gcpc.EventV1{
			Type:      "command.completed",
			Timestamp: 1,
			Data: &gcpc.EventV1_CommandPost{CommandPost: &gcpc.CommandPostEventV1{
				Command: "PING", ElapsedNs: 100, Result: "PONG",
			}},
		}},
	}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	pc := NewPluginConn("bench", transport.NewConn(server))
	reader := transport.NewConn(client)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < b.N; i++ {
			if _, err := reader.Recv(); err != nil {
				return
			}
		}
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pc.SendFireAndForget(env)
	}
	b.StopTimer()
	pc.Close()
	_ = reader.Close()
	<-done
}

// startMockPluginResponder reads command requests from conn and writes back
// command responses with matching request_id. Stops when conn closes or
// ctx is cancelled. This simulates a minimal plugin that immediately
// responds to every command.
func startMockPluginResponder(t testing.TB, conn *transport.Conn) (cancel func()) {
	t.Helper()
	ctx, cancelFunc := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			env, err := conn.Recv()
			if err != nil {
				return
			}
			req := env.GetCommandRequest()
			if req == nil {
				continue
			}
			respEnv := gcpc.NewCommandResponse(req.RequestId, &gcpc.ResultV1{
				Value: &gcpc.ResultV1_SimpleString{SimpleString: "OK"},
			}, false)
			if err := conn.Send(respEnv); err != nil {
				return
			}
		}
	}()
	return func() {
		cancelFunc()
		<-done
	}
}

func BenchmarkPluginCommandRTT_NetPipe(b *testing.B) {
	b.ReportAllocs()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	pc := NewPluginConn("bench-netpipe", transport.NewConn(server))
	go pc.StartReadLoop()
	cancel := startMockPluginResponder(b, transport.NewConn(client))
	defer cancel()
	defer pc.Close()

	ctx := context.Background()
	cmd := &gcpc.CommandInfoV1{Name: "PING"}
	connectionInfo := &gcpc.ConnectionInfoV1{Id: "bench-conn"}

	for i := 0; i < 100; i++ {
		reqID := NextRequestID()
		requestEnvelope := gcpc.NewCommandRequest(reqID, cmd, connectionInfo, nil, nil)
		respCh, err := pc.Send(ctx, requestEnvelope, reqID)
		if err != nil {
			b.Fatalf("warmup Send error: %v", err)
		}
		<-respCh
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reqID := NextRequestID()
		requestEnvelope := gcpc.NewCommandRequest(reqID, cmd, connectionInfo, nil, nil)
		respCh, err := pc.Send(ctx, requestEnvelope, reqID)
		if err != nil {
			b.Fatalf("Send error: %v", err)
		}
		<-respCh
	}
	b.StopTimer()
}

func BenchmarkPluginCommandRTT_AFUnix(b *testing.B) {
	b.ReportAllocs()

	sockPath := filepath.Join(b.TempDir(), "bench-afunix.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		b.Fatalf("Listen unix error: %v", err)
	}
	defer listener.Close()

	type unixDialOutcome struct {
		conn net.Conn
		err  error
	}
	unixDialCh := make(chan unixDialOutcome, 1)
	go func() {
		clientConn, dialErr := net.Dial("unix", sockPath)
		unixDialCh <- unixDialOutcome{clientConn, dialErr}
	}()
	server, err := listener.Accept()
	if err != nil {
		b.Fatalf("Accept error: %v", err)
	}
	defer server.Close()
	unixDial := <-unixDialCh
	if unixDial.err != nil {
		b.Fatalf("Dial error: %v", unixDial.err)
	}
	client := unixDial.conn
	defer client.Close()

	pc := NewPluginConn("bench-afunix", transport.NewConn(server))
	go pc.StartReadLoop()
	cancel := startMockPluginResponder(b, transport.NewConn(client))
	defer cancel()
	defer pc.Close()

	ctx := context.Background()
	cmd := &gcpc.CommandInfoV1{Name: "PING"}
	connectionInfo := &gcpc.ConnectionInfoV1{Id: "bench-conn"}

	for i := 0; i < 100; i++ {
		reqID := NextRequestID()
		requestEnvelope := gcpc.NewCommandRequest(reqID, cmd, connectionInfo, nil, nil)
		respCh, err := pc.Send(ctx, requestEnvelope, reqID)
		if err != nil {
			b.Fatalf("warmup Send error: %v", err)
		}
		<-respCh
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reqID := NextRequestID()
		requestEnvelope := gcpc.NewCommandRequest(reqID, cmd, connectionInfo, nil, nil)
		respCh, err := pc.Send(ctx, requestEnvelope, reqID)
		if err != nil {
			b.Fatalf("Send error: %v", err)
		}
		<-respCh
	}
	b.StopTimer()
}
