package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	gcpc "gocache/api/gcpc/v1"
	apiplugin "gocache/api/plugin"
	"gocache/commons/transport"
	"gocache/sdk/pluginsdk"
)

const (
	publishBenchChannel = "bench-channel"
	publishBenchMessage = "hello"
	publishBenchConnID  = "bench-conn"
	publishBenchReqID   = "publish-request"
)

type publishBenchmarkHarness struct {
	coreConn         *transport.Conn
	commandResponses chan *gcpc.CommandResponseV1
	readerDone       chan struct{}
	cancelPlugin     context.CancelFunc
	listener         net.Listener
	pluginDone       chan error
}

type acceptOutcome struct {
	conn net.Conn
	err  error
}

// BenchmarkPluginCommandPUBLISH measures FR-005: Exercises the real pubsub
// PUBLISH plugin command path. handlePublish calls PushToClient for each
// matching subscriber — each is a full GCPC IPC envelope send from plugin to
// core.
//
// Per-subscriber marginal cost = (RTT_N - RTT_0) / N. Delta method isolates
// the PushToClient IPC cost from fixed handlePublish overhead.
//
// MUST use real draining readers. If PushToClient sends are fire-and-forget
// into a buffer that never blocks, the benchmark measures send-enqueue cost not
// delivery cost.
//
// Do NOT assume linearity — the SubscriptionManager RLock and per-subscriber
// PushToClient contention may bend the curve.
func BenchmarkPluginCommandPUBLISH(b *testing.B) {
	subscriberCounts := []int{0, 1, 10, 100}
	for _, subscriberCount := range subscriberCounts {
		b.Run(fmt.Sprintf("Subscribers_%d", subscriberCount), func(b *testing.B) {
			publishHarness := startPublishBenchmarkHarness(b, subscriberCount)
			defer publishHarness.close(b)

			commandInfo := &gcpc.CommandInfoV1{Name: "PUBLISH", Args: []string{publishBenchChannel, publishBenchMessage}}
			connectionInfo := &gcpc.ConnectionInfoV1{Id: publishBenchConnID}

			b.ReportAllocs()
			b.ResetTimer()
			for iterationIndex := 0; iterationIndex < b.N; iterationIndex++ {
				requestEnvelope := gcpc.NewCommandRequest(publishBenchReqID, commandInfo, connectionInfo, nil, nil)
				if sendErr := publishHarness.coreConn.Send(requestEnvelope); sendErr != nil {
					b.Fatalf("send PUBLISH request: %v", sendErr)
				}

				commandResponse := <-publishHarness.commandResponses
				if commandResponse.RequestId != publishBenchReqID {
					b.Fatalf("command response request_id = %q, want %q", commandResponse.RequestId, publishBenchReqID)
				}
			}
			b.StopTimer()
		})
	}
}

func startPublishBenchmarkHarness(b *testing.B, subscriberCount int) *publishBenchmarkHarness {
	b.Helper()
	if runtime.GOOS == "windows" {
		b.Skip("pubsub plugin benchmark uses the Unix socket transport required by pluginsdk.Run")
	}

	socketPath := filepath.Join(b.TempDir(), "pubsub-publish-bench.sock")
	listener, listenErr := net.Listen("unix", socketPath)
	if listenErr != nil {
		b.Fatalf("listen unix socket: %v", listenErr)
	}
	b.Setenv(apiplugin.EnvSocketPath, socketPath)

	manager := NewSubscriptionManager()
	for subscriberIndex := 0; subscriberIndex < subscriberCount; subscriberIndex++ {
		manager.Subscribe(fmt.Sprintf("bench-sub-%d", subscriberIndex), publishBenchChannel)
	}

	pluginCtx, cancelPlugin := context.WithCancel(context.Background())
	pluginDone := make(chan error, 1)
	go func() {
		pluginDone <- pluginsdk.Run(pluginCtx, &PubSub{manager: manager})
	}()

	acceptedConns := make(chan acceptOutcome, 1)
	go func() {
		acceptedConn, acceptErr := listener.Accept()
		acceptedConns <- acceptOutcome{conn: acceptedConn, err: acceptErr}
	}()

	var rawCoreConn net.Conn
	select {
	case acceptedConn := <-acceptedConns:
		if acceptedConn.err != nil {
			cancelPlugin()
			b.Fatalf("accept plugin connection: %v", acceptedConn.err)
		}
		rawCoreConn = acceptedConn.conn
	case pluginErr := <-pluginDone:
		cancelPlugin()
		b.Fatalf("run pubsub plugin before accept: %v", pluginErr)
	case <-time.After(5 * time.Second):
		cancelPlugin()
		b.Fatal("timeout waiting for pubsub plugin connection")
	}

	coreConn := transport.NewConn(rawCoreConn)
	registerEnvelope, recvErr := coreConn.Recv()
	if recvErr != nil {
		cancelPlugin()
		closeBenchmarkConn(b, coreConn)
		closeBenchmarkListener(b, listener)
		b.Fatalf("receive register envelope: %v", recvErr)
	}
	if registerEnvelope.GetRegister() == nil {
		cancelPlugin()
		closeBenchmarkConn(b, coreConn)
		closeBenchmarkListener(b, listener)
		b.Fatalf("first plugin envelope = %T, want Register", registerEnvelope.Payload)
	}
	if ackErr := coreConn.Send(gcpc.NewRegisterAck(true, "", []string{"write", "hook:pre", "events"}, nil)); ackErr != nil {
		cancelPlugin()
		closeBenchmarkConn(b, coreConn)
		closeBenchmarkListener(b, listener)
		b.Fatalf("send register ack: %v", ackErr)
	}

	commandResponses := make(chan *gcpc.CommandResponseV1, 1)
	readerDone := make(chan struct{})
	go drainPubSubCoreEnvelopes(coreConn, commandResponses, readerDone)

	return &publishBenchmarkHarness{
		coreConn:         coreConn,
		commandResponses: commandResponses,
		readerDone:       readerDone,
		cancelPlugin:     cancelPlugin,
		listener:         listener,
		pluginDone:       pluginDone,
	}
}

func drainPubSubCoreEnvelopes(coreConn *transport.Conn, commandResponses chan<- *gcpc.CommandResponseV1, readerDone chan<- struct{}) {
	defer close(readerDone)
	for {
		envelope, recvErr := coreConn.Recv()
		if recvErr != nil {
			return
		}
		if commandResponse := envelope.GetCommandResponse(); commandResponse != nil {
			commandResponses <- commandResponse
		}
	}
}

func (h *publishBenchmarkHarness) close(b *testing.B) {
	b.Helper()
	closeBenchmarkConn(b, h.coreConn)
	h.cancelPlugin()
	closeBenchmarkListener(b, h.listener)
	<-h.readerDone

	select {
	case pluginErr := <-h.pluginDone:
		if pluginErr != nil && !errors.Is(pluginErr, context.Canceled) {
			b.Logf("pubsub plugin exited during benchmark cleanup: %v", pluginErr)
			return
		}
	case <-time.After(5 * time.Second):
		b.Log("timeout waiting for pubsub plugin cleanup")
		return
	}
}

func closeBenchmarkConn(b *testing.B, coreConn *transport.Conn) {
	b.Helper()
	if closeErr := coreConn.Close(); closeErr != nil {
		b.Logf("close benchmark transport connection: %v", closeErr)
	}
}

func closeBenchmarkListener(b *testing.B, listener net.Listener) {
	b.Helper()
	if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		b.Logf("close benchmark listener: %v", closeErr)
	}
}
