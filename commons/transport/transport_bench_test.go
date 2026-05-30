package transport

import (
	"net"
	"testing"

	gcpc "gocache/api/gcpc/v1"
)

func BenchmarkConnSendRecv(b *testing.B) {
	env := &gcpc.EnvelopeV1{
		Version: gcpc.ProtocolVersion,
		Payload: &gcpc.EnvelopeV1_Event{Event: &gcpc.EventV1{
			Type:        "command.completed",
			Timestamp:   1,
			OperationId: "cmd_1",
			Data: &gcpc.EventV1_CommandPost{CommandPost: &gcpc.CommandPostEventV1{
				Command:   "PING",
				ElapsedNs: 100,
				Result:    "PONG",
			}},
		}},
	}

	b.Run("send_recv_roundtrip", func(b *testing.B) {
		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()
		sender := NewConn(server)
		receiver := NewConn(client)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < b.N; i++ {
				if _, err := receiver.Recv(); err != nil {
					return
				}
			}
		}()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := sender.Send(env); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		<-done
		_ = sender.Close()
		_ = receiver.Close()
	})
}
