package server

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	apicommand "gocache/api/command"
	apievents "gocache/api/events"
	"gocache/commons/logger"
	commonobs "gocache/commons/observability"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/engine"
	serverEvents "gocache/pkg/events"
	"gocache/pkg/watch"
	"gocache/testkit/common/telemetrytest"
)

func TestIT_TelemetryDrainWorkerPublishesCommandTelemetry(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	eventBus := serverEvents.NewBus()
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           16,
		RecordsPerOperation:   16,
		CompletedRingPerShard: 16,
	})

	received := make(chan apievents.Event, 32)
	eventBus.Subscribe("test-command-telemetry", []apievents.Type{
		apievents.OperationStarted,
		apievents.OperationCompleted,
		apievents.CommandStarted,
		apievents.CommandCompleted,
	}, func(event apievents.Event) {
		received <- event
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	worker := NewOperationTrackerDrainWorker(manager, time.Millisecond)
	worker.SetEmitter(eventBus)
	worker.Start(ctx)
	t.Cleanup(worker.Stop)
	waiter := telemetrytest.NewEventWaiter(t, worker, received)

	srv := New("127.0.0.1:0", c, e, "", blocking.NewRegistry(), watch.NewManager())
	srv.SetEmitter(eventBus)
	srv.SetOperationTrackerManager(manager)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.listener = listener
	go srv.acceptConnections(ctx)
	t.Cleanup(func() { _ = srv.Shutdown(2 * time.Second) })

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if reply := sendCommand(t, conn, "PING"); reply.Str != "PONG" {
		t.Fatalf("PING reply = %q, want PONG", reply.Str)
	}

	operationStart := waiter.Wait("command operation.started", func(event apievents.Event) bool {
		payload := event.Proto.GetOperationStart()
		return event.Proto.Type == string(apievents.OperationStarted) && payload != nil && payload.Type == "command"
	})
	if operationStart.Proto.OperationId == "" {
		t.Fatal("command operation.started should carry operation_id")
	}

	commandStart := waiter.Wait("command.started PING", func(event apievents.Event) bool {
		payload := event.Proto.GetCommandPre()
		return event.Proto.Type == string(apievents.CommandStarted) && payload != nil && payload.Command == "PING"
	})
	if commandStart.Proto.OperationId == "" {
		t.Fatal("command.started should carry operation_id")
	}

	commandComplete := waiter.Wait("command.completed PING", func(event apievents.Event) bool {
		payload := event.Proto.GetCommandPost()
		return event.Proto.Type == string(apievents.CommandCompleted) && payload != nil && payload.Command == "PING"
	})
	if commandComplete.Proto.OperationId != commandStart.Proto.OperationId {
		t.Fatalf("command completion operation_id = %q, want %q", commandComplete.Proto.OperationId, commandStart.Proto.OperationId)
	}

	operationComplete := waiter.Wait("command operation.completed", func(event apievents.Event) bool {
		payload := event.Proto.GetOperationComplete()
		return event.Proto.Type == string(apievents.OperationCompleted) && payload != nil && payload.Type == "command"
	})
	if operationComplete.Proto.OperationId != operationStart.Proto.OperationId {
		t.Fatalf("operation completion operation_id = %q, want %q", operationComplete.Proto.OperationId, operationStart.Proto.OperationId)
	}
}

func TestIT_TelemetryDrainWorkerMaterializesRuntimeLogFromTCPReadError(t *testing.T) {
	var output lockedLogBuffer
	logger.InitWithWriter(&output, "debug")
	t.Cleanup(func() { logger.Init("info") })

	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	eventBus := serverEvents.NewBus()
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           16,
		RecordsPerOperation:   16,
		CompletedRingPerShard: 16,
	})

	received := make(chan apievents.Event, 32)
	eventBus.Subscribe("test-runtime-log-telemetry", []apievents.Type{
		apievents.OperationStarted,
		apievents.OperationCompleted,
	}, func(event apievents.Event) {
		received <- event
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	worker := NewOperationTrackerDrainWorker(manager, time.Millisecond)
	worker.SetEmitter(eventBus)
	worker.Start(ctx)
	t.Cleanup(worker.Stop)
	waiter := telemetrytest.NewEventWaiter(t, worker, received)

	srv := New("127.0.0.1:0", c, e, "", blocking.NewRegistry(), watch.NewManager())
	srv.SetEmitter(eventBus)
	srv.SetOperationTrackerManager(manager)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.listener = listener
	go srv.acceptConnections(ctx)
	t.Cleanup(func() { _ = srv.Shutdown(2 * time.Second) })

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("dial returned %T, want *net.TCPConn", conn)
	}
	if err := tcpConn.SetLinger(0); err != nil {
		t.Fatalf("set linger: %v", err)
	}
	if _, err := conn.Write([]byte("*2\r\n$4\r\nPING\r\n$4\r\n")); err != nil {
		t.Fatalf("write partial RESP frame: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close reset connection: %v", err)
	}

	runtimeStart := waiter.Wait("runtime.log operation.started", func(event apievents.Event) bool {
		payload := event.Proto.GetOperationStart()
		return event.Proto.Type == string(apievents.OperationStarted) && payload != nil && payload.Type == "runtime.log"
	})
	if runtimeStart.Proto.OperationId == "" {
		t.Fatal("runtime.log operation.started should carry operation_id")
	}

	runtimeComplete := waiter.Wait("runtime.log operation.completed", func(event apievents.Event) bool {
		payload := event.Proto.GetOperationComplete()
		return event.Proto.Type == string(apievents.OperationCompleted) && payload != nil && payload.Type == "runtime.log"
	})
	if runtimeComplete.Proto.OperationId != runtimeStart.Proto.OperationId {
		t.Fatalf("runtime.log completion operation_id = %q, want %q", runtimeComplete.Proto.OperationId, runtimeStart.Proto.OperationId)
	}

	entry := waitForLogMessage(t, &output, "connection read error")
	if entry["error"] == "" {
		t.Fatalf("runtime log entry missing error field: %+v", entry)
	}
	ctxFields := logContext(t, entry)
	if ctxFields[apicommand.ConnectionIDKey] == "" || ctxFields[apicommand.RemoteAddrKey] == "" {
		t.Fatalf("runtime log context = %+v, want connection id and remote addr", ctxFields)
	}
}

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForLogMessage(t *testing.T, output *lockedLogBuffer, message string) map[string]any {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		for _, entry := range decodeOptionalLogEntries(t, output.String()) {
			if entry["message"] == message {
				return entry
			}
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("timed out waiting for log message %q; logs: %s", message, output.String())
		}
	}
}

func decodeOptionalLogEntries(t *testing.T, lines string) []map[string]any {
	t.Helper()
	if strings.TrimSpace(lines) == "" {
		return nil
	}
	return decodeLogEntries(t, lines)
}
