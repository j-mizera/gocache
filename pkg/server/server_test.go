package server

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	apicommand "gocache/api/command"
	apievents "gocache/api/events"
	apiobs "gocache/api/observability"
	"gocache/commons/logger"
	commonobs "gocache/commons/observability"
	"gocache/commons/resp"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/engine"
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

func TestServer_SetOperationTrackerManagerNilKeepsCommandsWorking(t *testing.T) {
	srv, addr := startTestServer(t, "")
	srv.SetOperationTrackerManager(nil)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	val := sendCommand(t, conn, "PING")
	if val.Str != "PONG" {
		t.Fatalf("expected PONG, got %q", val.Str)
	}
}

func TestServer_SetOperationTrackerManagerThreadsCommandScope(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	srv := New("127.0.0.1:0", c, e, "", br, wm)
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           4,
		RecordsPerOperation:   2,
		CompletedRingPerShard: 4,
	})
	srv.SetOperationTrackerManager(manager)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv.listener = listener
	go srv.acceptConnections(context.Background())
	t.Cleanup(func() { _ = srv.Shutdown(2 * time.Second) })

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	val := sendCommand(t, conn, "PING")
	if val.Str != "PONG" {
		t.Fatalf("expected PONG, got %q", val.Str)
	}

	var completed commonobs.CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation commonobs.CompletedOperation) {
		if operation.Operation >= connectionOperationIdentityBase {
			return
		}
		if !completed.Operation.IsZero() {
			t.Fatalf("drained more than one command operation: prior=%d current=%d", completed.Operation, operation.Operation)
		}
		completed = operation
		if operation.ContextVersion.IsZero() {
			t.Fatal("completed command should carry a pinned connection context version")
		}
		if !manager.VisitConnectionContextVersion(operation.ContextVersion, nil) {
			t.Fatal("pinned connection context version should be visitable during drain")
		}
	}); drained != 2 {
		t.Fatalf("drained %d operations, want connection.started plus command", drained)
	}
	if completed.Operation.IsZero() {
		t.Fatal("completed command operation identity is zero")
	}
	if !completed.Parent.IsZero() {
		t.Fatal("completed command operation should not carry a telemetry parent through server context")
	}
	if completed.Status != commonobs.SlotTerminalFinished {
		t.Fatalf("command status = %v, want finished", completed.Status)
	}
	contextVersion := completed.ContextVersion
	if err := conn.Close(); err != nil {
		t.Fatalf("close connection: %v", err)
	}
	if err := srv.Shutdown(2 * time.Second); err != nil {
		t.Fatalf("shutdown after connection close: %v", err)
	}
	if drained := manager.DrainCompletedShard(0, func(commonobs.CompletedOperation) {}); drained != 1 {
		t.Fatalf("drained %d close operations, want 1", drained)
	}
	if manager.VisitConnectionContextVersion(contextVersion, nil) {
		t.Fatal("connection close drain should release the unpinned current context version")
	}
}

func TestServer_ConnectionLifecycleUsesSeparateTelemetryOperations(t *testing.T) {
	var output bytes.Buffer
	logger.InitWithWriter(&output, "debug")
	t.Cleanup(func() { logger.Init("info") })

	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })
	br := blocking.NewRegistry()
	wm := watch.NewManager()
	srv := New("127.0.0.1:0", c, e, "", br, wm)
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           2,
		RecordsPerOperation:   8,
		CompletedRingPerShard: 2,
	})

	const (
		connID     = "conn_101"
		remoteAddr = "127.0.0.1:6380"
	)
	connIdentity := apiobs.ConnectionIdentity(101)
	srv.recordConnectionStarted(manager, connIdentity, connID, remoteAddr)
	srv.recordConnectionClosed(manager, connIdentity, connID, remoteAddr, 123)

	emitter := &recordingEmitter{subscribed: true}
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(emitter)
	if drained := worker.DrainOnce(); drained != 2 {
		t.Fatalf("DrainOnce() = %d, want connection.started and connection.closed", drained)
	}

	gotTypes := make([]string, len(emitter.events))
	for i, event := range emitter.events {
		gotTypes[i] = event.Proto.Type
	}
	wantTypes := []string{
		string(apievents.OperationStarted),
		string(apievents.ConnectionOpen),
		string(apievents.OperationCompleted),
		string(apievents.OperationStarted),
		string(apievents.ConnectionClose),
		string(apievents.OperationCompleted),
	}
	if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	startOp := emitter.events[0].Proto.GetOperationStart()
	closeOp := emitter.events[3].Proto.GetOperationStart()
	if startOp == nil || startOp.Type != "connection.started" {
		t.Fatalf("start operation = %+v, want connection.started", startOp)
	}
	if closeOp == nil || closeOp.Type != "connection.closed" {
		t.Fatalf("close operation = %+v, want connection.closed", closeOp)
	}
	if startOp.Id == closeOp.Id {
		t.Fatalf("connection start and close should be distinct operations: %q", startOp.Id)
	}
	if open := emitter.events[1].Proto.GetConnectionOpen(); open == nil || open.ConnectionId != connID || open.RemoteAddr != remoteAddr {
		t.Fatalf("connection.open = %+v, want id=%s remote=%s", open, connID, remoteAddr)
	}
	if close := emitter.events[4].Proto.GetConnectionClose(); close == nil || close.ConnectionId != connID || close.RemoteAddr != remoteAddr || close.DurationNs != 123 {
		t.Fatalf("connection.close = %+v, want id=%s remote=%s duration=123", close, connID, remoteAddr)
	}

	entries := decodeLogEntries(t, output.String())
	if len(entries) != 2 {
		t.Fatalf("materialized logs = %d, want 2: %q", len(entries), output.String())
	}
	if entries[0]["message"] != connectionStartedLogMessage || entries[1]["message"] != connectionClosedLogMessage {
		t.Fatalf("log messages = %v / %v", entries[0]["message"], entries[1]["message"])
	}
	startedContext := logContext(t, entries[0])
	closedContext := logContext(t, entries[1])
	for name, ctx := range map[string]map[string]any{"started": startedContext, "closed": closedContext} {
		if ctx[apicommand.ConnectionIDKey] != connID || ctx[apicommand.RemoteAddrKey] != remoteAddr {
			t.Fatalf("%s log context = %+v, want connection id and remote addr", name, ctx)
		}
	}
}

func TestServer_RuntimeLogUsesTelemetryOperation(t *testing.T) {
	var output bytes.Buffer
	logger.InitWithWriter(&output, "debug")
	t.Cleanup(func() { logger.Init("info") })

	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })
	srv := New("127.0.0.1:0", c, e, "", blocking.NewRegistry(), watch.NewManager())
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   4,
		CompletedRingPerShard: 1,
	})
	connIdentity := apiobs.ConnectionIdentity(202)
	manager.UpdateConnectionContextStrings(
		connIdentity,
		apicommand.RemoteAddrKey, "127.0.0.1:6381",
		apicommand.ConnectionIDKey, "conn_202",
	)
	if !srv.recordRuntimeLog(manager, connIdentity, apiobs.TelemetryLogLevelWarn, "connection read error", "error", "boom") {
		t.Fatal("runtime log should submit through telemetry operation")
	}

	emitter := &recordingEmitter{subscribed: true}
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(emitter)
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want one runtime.log operation", drained)
	}
	gotTypes := make([]string, len(emitter.events))
	for i, event := range emitter.events {
		gotTypes[i] = event.Proto.Type
	}
	wantTypes := []string{string(apievents.OperationStarted), string(apievents.OperationCompleted)}
	if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	entry := decodeSingleLogEntry(t, output.String())
	if entry["message"] != "connection read error" || entry["error"] != "boom" {
		t.Fatalf("log entry = %+v, want message and error field", entry)
	}
	ctx := logContext(t, entry)
	if ctx[apicommand.ConnectionIDKey] != "conn_202" || ctx[apicommand.RemoteAddrKey] != "127.0.0.1:6381" {
		t.Fatalf("log context = %+v, want connection id and remote", ctx)
	}
}

func TestServer_AuthFailedUsesTelemetryEvent(t *testing.T) {
	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })
	srv := New("127.0.0.1:0", c, e, "", blocking.NewRegistry(), watch.NewManager())
	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   4,
		CompletedRingPerShard: 1,
	})
	const (
		remoteAddr = "127.0.0.1:6382"
		command    = "PING"
	)
	connIdentity := apiobs.ConnectionIdentity(203)
	manager.UpdateConnectionContextStrings(connIdentity, apicommand.RemoteAddrKey, remoteAddr, apicommand.ConnectionIDKey, "conn_203")
	if !srv.recordAuthFailed(manager, connIdentity, remoteAddr, command) {
		t.Fatal("auth failed should submit through telemetry operation")
	}

	emitter := &recordingEmitter{subscribed: true}
	worker := NewOperationTrackerDrainWorker(manager, time.Hour)
	worker.SetEmitter(emitter)
	if drained := worker.DrainOnce(); drained != 1 {
		t.Fatalf("DrainOnce() = %d, want one auth.failed operation", drained)
	}
	gotTypes := make([]string, len(emitter.events))
	for i, event := range emitter.events {
		gotTypes[i] = event.Proto.Type
	}
	wantTypes := []string{string(apievents.OperationStarted), string(apievents.AuthFailed), string(apievents.OperationCompleted)}
	if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	failed := emitter.events[1].Proto.GetAuthFailed()
	if failed == nil || failed.RemoteAddr != remoteAddr || failed.Command != command {
		t.Fatalf("auth.failed = %+v, want remote=%s command=%s", failed, remoteAddr, command)
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

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx, StartupTelemetry{}) }()

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

func TestServer_StartCompletesStartupTelemetryAfterListen(t *testing.T) {
	var output bytes.Buffer
	logger.InitWithWriter(&output, "debug")
	t.Cleanup(func() { logger.Init("info") })

	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	srv := New("127.0.0.1:0", c, e, "", br, wm)

	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   2,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(44, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate startup slot")
	}
	scope := commonobs.NewOperationScope(manager, handle, 44, apiobs.NewOperationRef("startup-public", ""))
	ready := make(chan struct{})
	startup := StartupTelemetry{
		Scope: scope,
		Logs:  commonobs.StartupLogMaterializer{},
		OnReady: func() {
			close(ready)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx, startup) }()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not report startup readiness")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Start returned %v, want context canceled or nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after cancel")
	}

	line := output.String()
	for _, want := range []string{`"level":"info"`, `"_operation_id":"startup-public"`, `"message":"server listening"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("materialized server log %q missing %s", line, want)
		}
	}

	var completed commonobs.CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation commonobs.CompletedOperation) {
		completed = operation
		completed.Records = append([]apiobs.TelemetryRecord(nil), operation.Records...)
	}); drained != 1 {
		t.Fatalf("drained %d operations, want 1", drained)
	}
	if completed.Status != commonobs.SlotTerminalFinished {
		t.Fatalf("startup status = %v, want finished", completed.Status)
	}
	if len(completed.Records) != 1 {
		t.Fatalf("startup record count = %d, want 1", len(completed.Records))
	}
	record := completed.Records[0]
	if record.Kind != apiobs.TelemetryRecordLog || string(record.NameBytes()) != "server listening" {
		t.Fatalf("record kind/message = %v/%q, want log/server listening", record.Kind, record.NameBytes())
	}
	if record.Flags&apiobs.TelemetryRecordFlagLocalLogMaterialized == 0 {
		t.Fatal("server listening record should be marked local-materialized")
	}
}

func TestServer_StartFailsStartupTelemetryOnListenError(t *testing.T) {
	var output bytes.Buffer
	logger.InitWithWriter(&output, "debug")
	t.Cleanup(func() { logger.Init("info") })

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listen addr: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })

	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	srv := New(occupied.Addr().String(), c, e, "", br, wm)

	manager := commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   2,
		CompletedRingPerShard: 1,
	})
	handle, ok := manager.StartOperation(45, apiobs.ParentRef{}, 0)
	if !ok {
		t.Fatal("StartOperation should allocate startup slot")
	}
	scope := commonobs.NewOperationScope(manager, handle, 45, apiobs.NewOperationRef("startup-failure", ""))
	readyCalled := false
	startup := StartupTelemetry{
		Scope: scope,
		Logs:  commonobs.StartupLogMaterializer{},
		OnReady: func() {
			readyCalled = true
		},
	}

	err = srv.Start(context.Background(), startup)
	if err == nil {
		t.Fatal("Start returned nil, want listen error")
	}
	if !strings.Contains(err.Error(), "listen "+occupied.Addr().String()) {
		t.Fatalf("Start error = %v, want wrapped listen addr", err)
	}
	if readyCalled {
		t.Fatal("OnReady should not run after listen failure")
	}

	line := output.String()
	for _, want := range []string{`"level":"error"`, `"_operation_id":"startup-failure"`, `"message":"server listen failed"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("materialized failure log %q missing %s", line, want)
		}
	}

	var completed commonobs.CompletedOperation
	if drained := manager.DrainCompletedShard(0, func(operation commonobs.CompletedOperation) {
		completed = operation
		completed.Records = append([]apiobs.TelemetryRecord(nil), operation.Records...)
	}); drained != 1 {
		t.Fatalf("drained %d operations, want 1", drained)
	}
	if completed.Status != commonobs.SlotTerminalFailed {
		t.Fatalf("startup status = %v, want failed", completed.Status)
	}
	if len(completed.Records) != 1 {
		t.Fatalf("startup record count = %d, want 1", len(completed.Records))
	}
	record := completed.Records[0]
	if record.Kind != apiobs.TelemetryRecordLog || record.Level != apiobs.TelemetryLogLevelError || string(record.NameBytes()) != "server listen failed" {
		t.Fatalf("record kind/level/message = %v/%v/%q, want log/error/server listen failed", record.Kind, record.Level, record.NameBytes())
	}
	if record.Flags&apiobs.TelemetryRecordFlagLocalLogMaterialized == 0 {
		t.Fatal("server listen failed record should be marked local-materialized")
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
