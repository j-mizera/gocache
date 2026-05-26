package transport

import (
	"encoding/binary"
	gcpc "gocache/api/gcpc/v1"
	"net"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
)

func connPair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	s, c := net.Pipe()
	return NewConn(s), NewConn(c)
}

func TestSendRecv_Roundtrip(t *testing.T) {
	server, client := connPair(t)
	defer server.Close()
	defer client.Close()

	env := &gcpc.EnvelopeV1{
		Id: 42,
		Payload: &gcpc.EnvelopeV1_Register{
			Register: &gcpc.RegisterV1{Name: "test", Version: "1.0", Critical: true},
		},
	}

	go func() {
		if err := client.Send(env); err != nil {
			t.Errorf("send: %v", err)
		}
	}()

	got, err := server.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}

	if got.Id != 42 {
		t.Errorf("expected id 42, got %d", got.Id)
	}
	reg := got.GetRegister()
	if reg == nil {
		t.Fatal("expected Register payload")
	}
	if reg.Name != "test" {
		t.Errorf("expected name 'test', got %q", reg.Name)
	}
}

func TestRecv_ConnClosed(t *testing.T) {
	server, client := connPair(t)
	client.Close()

	_, err := server.Recv()
	if err != ErrConnClosed {
		t.Errorf("expected ErrConnClosed, got %v", err)
	}
	server.Close()
}

func TestRecv_FrameTooLarge(t *testing.T) {
	server, client := connPair(t)
	defer server.Close()
	defer client.Close()

	go func() {
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, MaxFrameSize+1)
		client.conn.Write(header)
	}()

	_, err := server.Recv()
	if err != ErrFrameTooLarge {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestSend_FrameTooLarge(t *testing.T) {
	server, client := connPair(t)
	defer server.Close()
	defer client.Close()

	bigName := make([]byte, MaxFrameSize+1)
	for i := range bigName {
		bigName[i] = 'a'
	}
	env := &gcpc.EnvelopeV1{
		Id: 1,
		Payload: &gcpc.EnvelopeV1_Register{
			Register: &gcpc.RegisterV1{Name: string(bigName)},
		},
	}

	data, _ := proto.Marshal(env)
	if len(data) <= MaxFrameSize {
		t.Skip("test envelope not large enough")
	}

	err := server.Send(env)
	if err != ErrFrameTooLarge {
		t.Errorf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestConcurrentSends(t *testing.T) {
	server, client := connPair(t)
	defer server.Close()
	defer client.Close()

	const n = 50
	var wg sync.WaitGroup

	for i := range n {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			env := &gcpc.EnvelopeV1{
				Id:      id,
				Payload: &gcpc.EnvelopeV1_HealthCheck{HealthCheck: &gcpc.HealthCheckV1{Timestamp: id}},
			}
			if err := client.Send(env); err != nil {
				t.Errorf("send %d: %v", id, err)
			}
		}(uint64(i))
	}

	go func() {
		wg.Wait()
		client.Close()
	}()

	count := 0
	for {
		_, err := server.Recv()
		if err != nil {
			break
		}
		count++
	}

	if count != n {
		t.Errorf("expected %d messages, got %d", n, count)
	}
}

func TestListener_AcceptAndCleanup(t *testing.T) {
	sockPath := t.TempDir() + "/test.sock"

	ln, err := NewListener(sockPath)
	if err != nil {
		t.Fatalf("new listener: %v", err)
	}

	go func() {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Errorf("dial: %v", err)
			return
		}
		conn.Close()
	}()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	conn.Close()

	if ln.Addr() != sockPath {
		t.Errorf("expected addr %q, got %q", sockPath, ln.Addr())
	}

	ln.Close()
}
