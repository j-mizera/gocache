package router

import (
	"net"
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
