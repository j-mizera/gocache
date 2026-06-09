package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	gcpc "gocache/api/gcpc/v1"
	"gocache/commons/transport"
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
	if r.HasCommand("PUBLISH") {
		t.Error("expected PUBLISH to NOT be registered since it's a core command")
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
	go r.GetPluginConn("echo").StartReadLoop()

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
		if req.Command.GetName() != "ECHO" {
			t.Errorf("expected command ECHO, got %s", req.Command.GetName())
		}
		if len(req.Command.GetArgs()) != 1 || req.Command.GetArgs()[0] != "hello" {
			t.Errorf("unexpected args: %v", req.Command.GetArgs())
		}
		// Send response.
		result := &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: "hello"}}
		resp := gcpc.NewCommandResponse(req.RequestId, result, false)
		if err := clientConn.Send(resp); err != nil {
			t.Errorf("plugin send: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	val, _, err := r.Route(ctx, "ECHO", []string{"hello"}, nil, nil)
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
	_, _, err := r.Route(ctx, "EXACT", []string{"one"}, nil, nil)
	if err == nil {
		t.Error("expected arg validation error for too few args")
	}

	// Too many args.
	_, _, err = r.Route(ctx, "EXACT", []string{"one", "two", "three"}, nil, nil)
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

	_, _, err := r.Route(ctx, "SLOW", nil, nil, nil)
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
	go r.GetPluginConn("pinger").StartReadLoop()

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
			_ = clientConn.Send(gcpc.NewCommandResponse(req.RequestId, result, false))
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
			_, _, err := r.Route(ctx, "PING", nil, nil, nil)
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

func TestRouteMetadataForwarded(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	decls := []*gcpc.CommandDeclV1{
		{Name: "ECHO", MinArgs: 1, MaxArgs: 1},
	}
	if err := r.RegisterPlugin("echo", serverConn, decls); err != nil {
		t.Fatal(err)
	}
	go r.GetPluginConn("echo").StartReadLoop()

	// Plugin side: verify metadata arrives, send response.
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
		// Verify metadata.
		if req.Metadata == nil {
			t.Error("expected metadata to be non-nil")
		} else {
			if req.Metadata["traceparent"] != "00-abc-def-01" {
				t.Errorf("expected traceparent '00-abc-def-01', got %q", req.Metadata["traceparent"])
			}
			if req.Metadata["tenant"] != "acme" {
				t.Errorf("expected tenant 'acme', got %q", req.Metadata["tenant"])
			}
		}
		result := &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: "hello"}}
		resp := gcpc.NewCommandResponse(req.RequestId, result, false)
		_ = clientConn.Send(resp)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	metadata := map[string]string{
		"traceparent": "00-abc-def-01",
		"tenant":      "acme",
	}
	val, _, err := r.Route(ctx, "ECHO", []string{"hello"}, metadata, nil)
	if err != nil {
		t.Fatalf("Route error: %v", err)
	}
	if val != "hello" {
		t.Errorf("expected 'hello', got %v", val)
	}

	wg.Wait()
	clientConn.Close()
}

func TestRouteWithContextForwardsFilteredOperationContext(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	decls := []*gcpc.CommandDeclV1{
		{Name: "ECHO", MinArgs: 1, MaxArgs: 1},
	}
	if err := r.RegisterPlugin("echo", serverConn, decls); err != nil {
		t.Fatal(err)
	}
	go r.GetPluginConn("echo").StartReadLoop()

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
		if req.Context["_operation_id"] != "cmd_1" {
			t.Errorf("server context _operation_id = %q, want cmd_1", req.Context["_operation_id"])
		}
		if req.Context["shared.rex.traceparent"] != "00-abc" {
			t.Errorf("shared context traceparent = %q, want 00-abc", req.Context["shared.rex.traceparent"])
		}
		if req.Context["echo.private"] != "visible" {
			t.Errorf("own private context = %q, want visible", req.Context["echo.private"])
		}
		if _, ok := req.Context["other.private"]; ok {
			t.Errorf("other plugin private context leaked: %v", req.Context)
		}
		if _, ok := req.Context["shared.secret.token"]; ok {
			t.Errorf("shared secret context leaked: %v", req.Context)
		}
		if _, ok := req.Context["echo.secret.token"]; ok {
			t.Errorf("plugin secret context leaked: %v", req.Context)
		}
		result := &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: "hello"}}
		_ = clientConn.Send(gcpc.NewCommandResponse(req.RequestId, result, false))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	operationContext := map[string]string{
		"_operation_id":          "cmd_1",
		"shared.rex.traceparent": "00-abc",
		"shared.secret.token":    "hidden",
		"echo.private":           "visible",
		"echo.secret.token":      "hidden",
		"other.private":          "hidden",
	}
	val, _, err := r.RouteWithContext(ctx, "ECHO", []string{"hello"}, nil, nil, operationContext)
	if err != nil {
		t.Fatalf("RouteWithContext error: %v", err)
	}
	if val != "hello" {
		t.Errorf("expected 'hello', got %v", val)
	}

	wg.Wait()
	clientConn.Close()
}

func TestRouteNilMetadata(t *testing.T) {
	r := NewRouter([]string{"GET"})
	serverConn, clientConn := testPipe()
	defer serverConn.Close()

	decls := []*gcpc.CommandDeclV1{
		{Name: "ECHO", MinArgs: 1, MaxArgs: 1},
	}
	if err := r.RegisterPlugin("echo", serverConn, decls); err != nil {
		t.Fatal(err)
	}
	go r.GetPluginConn("echo").StartReadLoop()

	// Plugin side: verify metadata is nil/empty.
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
		if len(req.Metadata) != 0 {
			t.Errorf("expected empty metadata, got %v", req.Metadata)
		}
		result := &gcpc.ResultV1{Value: &gcpc.ResultV1_BulkString{BulkString: "hello"}}
		_ = clientConn.Send(gcpc.NewCommandResponse(req.RequestId, result, false))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := r.Route(ctx, "ECHO", []string{"hello"}, nil, nil)
	if err != nil {
		t.Fatalf("Route error: %v", err)
	}

	wg.Wait()
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

func TestPluginConnFireAndForgetReturnsWithoutReader(t *testing.T) {
	serverConn, clientConn := testPipe()
	defer clientConn.Close()

	pc := NewPluginConn("audit", serverConn)
	defer pc.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		pc.SendFireAndForget(gcpc.NewOperationHookRequest("req-ff", "op_1", "command", "", "complete", nil))
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("SendFireAndForget blocked without a plugin reader")
	}
}

func TestPluginConnFireAndForgetFIFO(t *testing.T) {
	serverConn, clientConn := testPipe()
	defer clientConn.Close()

	pc := NewPluginConn("audit", serverConn)
	defer pc.Close()

	const n = 3
	for i := 0; i < n; i++ {
		reqID := fmt.Sprintf("req-ff-%d", i)
		pc.SendFireAndForget(gcpc.NewOperationHookRequest(reqID, fmt.Sprintf("op_%d", i), "command", "", "complete", nil))
	}

	for i := 0; i < n; i++ {
		envCh := make(chan *gcpc.EnvelopeV1, 1)
		errCh := make(chan error, 1)
		go func() {
			env, err := clientConn.Recv()
			if err != nil {
				errCh <- err
				return
			}
			envCh <- env
		}()

		select {
		case env := <-envCh:
			req := env.GetOperationHookRequest()
			if req == nil {
				t.Fatalf("envelope[%d] is not an OperationHookRequest", i)
			}
			wantReqID := fmt.Sprintf("req-ff-%d", i)
			if req.RequestId != wantReqID {
				t.Fatalf("envelope[%d] request_id=%q, want %q", i, req.RequestId, wantReqID)
			}
		case err := <-errCh:
			t.Fatalf("recv envelope[%d]: %v", i, err)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for envelope[%d]", i)
		}
	}
}

func TestPluginConnWriteOutboundBatchPreservesFrameOrderAndStats(t *testing.T) {
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	pc := &PluginConn{
		conn: serverConn,
		Name: "audit",
	}
	batch := []outboundEnvelope{
		{env: gcpc.NewHealthCheck()},
		{env: gcpc.NewOperationHookRequest("req-1", "op_1", "command", "", "complete", nil)},
		{build: func() *gcpc.EnvelopeV1 {
			return gcpc.NewOperationHookRequest("req-2", "op_2", "command", "", "complete", nil)
		}},
	}

	errCh := make(chan struct{}, 1)
	go func() {
		pc.writeOutboundBatch(batch)
		close(errCh)
	}()

	if env, err := clientConn.Recv(); err != nil {
		t.Fatalf("recv health: %v", err)
	} else if env.GetHealthCheck() == nil {
		t.Fatalf("first envelope is %T, want HealthCheck", env.Payload)
	}
	for i, wantReqID := range []string{"req-1", "req-2"} {
		env, err := clientConn.Recv()
		if err != nil {
			t.Fatalf("recv operation hook %d: %v", i, err)
		}
		req := env.GetOperationHookRequest()
		if req == nil {
			t.Fatalf("envelope %d is %T, want OperationHookRequest", i+1, env.Payload)
		}
		if req.RequestId != wantReqID {
			t.Fatalf("request_id=%q, want %q", req.RequestId, wantReqID)
		}
	}
	<-errCh

	stats := pc.Stats()
	if stats.WriteAttempts != 3 {
		t.Fatalf("WriteAttempts=%d, want 3", stats.WriteAttempts)
	}
	if stats.WriteBatches != 1 {
		t.Fatalf("WriteBatches=%d, want 1", stats.WriteBatches)
	}
	if stats.WriteBatchEnvelopes != 3 {
		t.Fatalf("WriteBatchEnvelopes=%d, want 3", stats.WriteBatchEnvelopes)
	}
	if stats.WriteBatchMaxSize != 3 {
		t.Fatalf("WriteBatchMaxSize=%d, want 3", stats.WriteBatchMaxSize)
	}
}

func TestPluginConnWriteOutboundBatchSkipsNilFireAndForgetEnvelope(t *testing.T) {
	serverConn, clientConn := testPipe()
	defer clientConn.Close()
	defer serverConn.Close()

	pc := &PluginConn{
		conn: serverConn,
		Name: "audit",
	}
	blockingErrCh := make(chan error, 1)
	batch := []outboundEnvelope{
		{build: func() *gcpc.EnvelopeV1 { return nil }},
		{env: gcpc.NewOperationHookRequest("req-valid", "op_1", "command", "", "complete", nil), errCh: blockingErrCh},
	}

	done := make(chan struct{})
	go func() {
		pc.writeOutboundBatch(batch)
		close(done)
	}()

	env, err := clientConn.Recv()
	if err != nil {
		t.Fatalf("recv valid envelope: %v", err)
	}
	req := env.GetOperationHookRequest()
	if req == nil {
		t.Fatalf("received %T, want OperationHookRequest", env.Payload)
	}
	if req.RequestId != "req-valid" {
		t.Fatalf("request_id=%q, want req-valid", req.RequestId)
	}

	<-done
	if err := <-blockingErrCh; err != nil {
		t.Fatalf("valid blocking item error=%v, want nil", err)
	}
	stats := pc.Stats()
	if stats.WriteAttempts != 2 {
		t.Fatalf("WriteAttempts=%d, want 2", stats.WriteAttempts)
	}
	if stats.WriteErrors != 1 {
		t.Fatalf("WriteErrors=%d, want 1 nil-envelope error", stats.WriteErrors)
	}
	if stats.WriteBatchEnvelopes != 1 {
		t.Fatalf("WriteBatchEnvelopes=%d, want 1", stats.WriteBatchEnvelopes)
	}
	if stats.WriteBatchMaxSize != 1 {
		t.Fatalf("WriteBatchMaxSize=%d, want 1", stats.WriteBatchMaxSize)
	}
}

func TestPluginConnWriteOutboundBatchReturnsErrorForNilBlockingEnvelope(t *testing.T) {
	pc := &PluginConn{Name: "audit"}
	errCh := make(chan error, 1)

	pc.writeOutboundBatch([]outboundEnvelope{{errCh: errCh}})

	if err := <-errCh; !errors.Is(err, errNilOutboundEnvelope) {
		t.Fatalf("blocking nil envelope error=%v, want errNilOutboundEnvelope", err)
	}
	stats := pc.Stats()
	if stats.WriteAttempts != 1 {
		t.Fatalf("WriteAttempts=%d, want 1", stats.WriteAttempts)
	}
	if stats.WriteErrors != 1 {
		t.Fatalf("WriteErrors=%d, want 1", stats.WriteErrors)
	}
	if stats.WriteBatchEnvelopes != 0 {
		t.Fatalf("WriteBatchEnvelopes=%d, want 0", stats.WriteBatchEnvelopes)
	}
}

func TestPluginConnCollectBatchFlushesWhenBlockingSendArrives(t *testing.T) {
	pc := &PluginConn{
		outbound: make(chan outboundEnvelope, pluginOutboundBatchMax),
		done:     make(chan struct{}),
		Name:     "metrics",
	}
	blockingErrCh := make(chan error, 1)
	resultCh := make(chan []outboundEnvelope, 1)

	start := time.Now()
	go func() {
		resultCh <- pc.collectBatchWithDelay(outboundEnvelope{env: gcpc.NewHealthCheck()}, 500*time.Millisecond)
	}()

	time.Sleep(10 * time.Millisecond)
	pc.outbound <- outboundEnvelope{env: gcpc.NewHealthCheck(), errCh: blockingErrCh}

	select {
	case batch := <-resultCh:
		if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
			t.Fatalf("collectBatch returned after %v, want prompt flush before telemetry delay", elapsed)
		}
		if len(batch) != 2 {
			t.Fatalf("len(batch)=%d, want 2", len(batch))
		}
		if batch[1].errCh != blockingErrCh {
			t.Fatal("blocking envelope was not preserved in the collected batch")
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("collectBatch waited for telemetry delay after blocking send arrived")
	}
}

func TestPluginConnLazyFireAndForgetSkipsBuildWhenQueueFull(t *testing.T) {
	pc := &PluginConn{
		outbound: make(chan outboundEnvelope, 1),
		done:     make(chan struct{}),
		Name:     "metrics",
	}

	built := 0
	builder := func() *gcpc.EnvelopeV1 {
		built++
		return gcpc.NewHealthCheck()
	}
	pc.SendFireAndForgetLazy(builder)
	pc.SendFireAndForgetLazy(builder)

	if built != 0 {
		t.Fatalf("lazy builder ran during enqueue/drop: built=%d", built)
	}
	stats := pc.Stats()
	if stats.SendAccepted != 1 {
		t.Fatalf("SendAccepted=%d, want 1", stats.SendAccepted)
	}
	if stats.SendQueueFull != 1 {
		t.Fatalf("SendQueueFull=%d, want 1", stats.SendQueueFull)
	}
	if stats.FireAndForgetDrops != 1 {
		t.Fatalf("FireAndForgetDrops=%d, want 1", stats.FireAndForgetDrops)
	}
}

func TestPluginConnHeadroom(t *testing.T) {
	pc := &PluginConn{outbound: make(chan outboundEnvelope, 2)}
	if got := pc.Headroom(); got != 2 {
		t.Fatalf("Headroom()=%d, want 2", got)
	}
	pc.outbound <- outboundEnvelope{env: gcpc.NewHealthCheck()}
	if got := pc.Headroom(); got != 1 {
		t.Fatalf("Headroom() after enqueue=%d, want 1", got)
	}
	pc.outbound = nil
	if got := pc.Headroom(); got != 0 {
		t.Fatalf("Headroom() with nil outbound=%d, want 0", got)
	}
	var nilConn *PluginConn
	if got := nilConn.Headroom(); got != 0 {
		t.Fatalf("nil Headroom()=%d, want 0", got)
	}
}

func TestPluginConnStatsTracksFireAndForgetDrops(t *testing.T) {
	pc := &PluginConn{
		outbound: make(chan outboundEnvelope, 1),
		done:     make(chan struct{}),
		Name:     "metrics",
	}

	pc.SendFireAndForget(gcpc.NewHealthCheck())
	pc.SendFireAndForget(gcpc.NewHealthCheck())

	stats := pc.Stats()
	if stats.SendAttempts != 2 {
		t.Fatalf("SendAttempts=%d, want 2", stats.SendAttempts)
	}
	if stats.SendAccepted != 1 {
		t.Fatalf("SendAccepted=%d, want 1", stats.SendAccepted)
	}
	if stats.SendQueueFull != 1 {
		t.Fatalf("SendQueueFull=%d, want 1", stats.SendQueueFull)
	}
	if stats.FireAndForgetAttempts != 2 {
		t.Fatalf("FireAndForgetAttempts=%d, want 2", stats.FireAndForgetAttempts)
	}
	if stats.FireAndForgetAccepted != 1 {
		t.Fatalf("FireAndForgetAccepted=%d, want 1", stats.FireAndForgetAccepted)
	}
	if stats.FireAndForgetDrops != 1 {
		t.Fatalf("FireAndForgetDrops=%d, want 1", stats.FireAndForgetDrops)
	}
	if stats.QueueDepth != 1 || stats.QueueCapacity != 1 {
		t.Fatalf("queue depth/capacity=%d/%d, want 1/1", stats.QueueDepth, stats.QueueCapacity)
	}
	if stats.QueueHeadroom != 0 {
		t.Fatalf("QueueHeadroom=%d, want 0", stats.QueueHeadroom)
	}
}

func TestPluginConnStatsTracksClosedAndCancelledDrops(t *testing.T) {
	pc := &PluginConn{
		outbound: make(chan outboundEnvelope, 1),
		done:     make(chan struct{}),
		Name:     "metrics",
	}
	pc.sendMu.Lock()
	pc.closed = true
	pc.sendMu.Unlock()

	pc.SendFireAndForget(gcpc.NewHealthCheck())
	stats := pc.Stats()
	if stats.SendPluginDown != 1 {
		t.Fatalf("SendPluginDown=%d, want 1", stats.SendPluginDown)
	}
	if stats.FireAndForgetDrops != 1 {
		t.Fatalf("FireAndForgetDrops=%d, want 1", stats.FireAndForgetDrops)
	}

	pc = &PluginConn{
		outbound: make(chan outboundEnvelope, 1),
		done:     make(chan struct{}),
		Name:     "metrics",
	}
	pc.SendFireAndForget(gcpc.NewHealthCheck())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pc.enqueue(ctx, outboundEnvelope{env: gcpc.NewHealthCheck()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("enqueue error=%v, want context.Canceled", err)
	}
	stats = pc.Stats()
	if stats.SendContextCancelled != 1 {
		t.Fatalf("SendContextCancelled=%d, want 1", stats.SendContextCancelled)
	}
	if stats.FireAndForgetDrops != 1 {
		t.Fatalf("FireAndForgetDrops=%d, want 1", stats.FireAndForgetDrops)
	}
}

func TestRouterPluginStatsSorted(t *testing.T) {
	r := NewRouter(nil)
	r.mu.Lock()
	r.plugins["zeta"] = &PluginConn{outbound: make(chan outboundEnvelope, 1), Name: "zeta"}
	r.plugins["alpha"] = &PluginConn{outbound: make(chan outboundEnvelope, 1), Name: "alpha"}
	r.mu.Unlock()

	stats := r.PluginStats()
	if len(stats) != 2 {
		t.Fatalf("len(stats)=%d, want 2", len(stats))
	}
	if stats[0].PluginName != "alpha" || stats[1].PluginName != "zeta" {
		t.Fatalf("stats order=%q,%q; want alpha,zeta", stats[0].PluginName, stats[1].PluginName)
	}
}

func TestPluginConnSendAfterCloseReturnsPluginDown(t *testing.T) {
	serverConn, clientConn := testPipe()
	defer clientConn.Close()

	pc := NewPluginConn("closed", serverConn)
	pc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := pc.Send(ctx, gcpc.NewOperationHookRequest("req-closed", "op_1", "command", "", "start", nil), "req-closed")
	if !errors.Is(err, ErrPluginDown) {
		t.Fatalf("Send error=%v, want ErrPluginDown", err)
	}

	pc.SendFireAndForget(gcpc.NewOperationHookRequest("req-closed-ff", "op_1", "command", "", "complete", nil))
}

func TestPluginConnPendingSendUnblocksOnClose(t *testing.T) {
	serverConn, clientConn := testPipe()
	defer clientConn.Close()

	pc := NewPluginConn("blocked", serverConn)

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := pc.Send(ctx, gcpc.NewOperationHookRequest("req-blocked", "op_1", "command", "", "start", nil), "req-blocked")
		errCh <- err
	}()

	go func() {
		time.Sleep(20 * time.Millisecond)
		pc.Close()
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Send returned nil error after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Send did not unblock after Close")
	}
}

func TestPluginConnConcurrentDeliverAndCloseDoesNotPanic(t *testing.T) {
	serverConn, clientConn := testPipe()
	defer clientConn.Close()

	pc := NewPluginConn("race", serverConn)
	defer pc.Close()

	const n = 100
	for i := 0; i < n; i++ {
		reqID := fmt.Sprintf("req-race-%d", i)
		ch := make(chan *gcpc.EnvelopeV1, 1)
		pc.storePending(reqID, ch)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			reqID := fmt.Sprintf("req-race-%d", i)
			wg.Add(1)
			go func() {
				defer wg.Done()
				pc.Deliver(reqID, gcpc.NewOperationHookResponse(reqID, nil))
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			pc.Close()
		}()
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent Deliver and Close did not finish")
	}
}

// TestPluginConnOperationHookRoundtrip guards against a regression where
// PluginConn.readLoop did not route OperationHookResponseV1 envelopes to
// pending channels, causing every synchronous operation start hook to
// block until its timeout before falling open.
func TestPluginConnOperationHookRoundtrip(t *testing.T) {
	serverConn, clientConn := testPipe()
	defer serverConn.Close()
	defer clientConn.Close()

	pc := NewPluginConn("op-hook-plugin", serverConn)
	defer pc.Close()
	go pc.StartReadLoop()

	// Simulated plugin: when it sees an OperationHookRequest, reply immediately.
	go func() {
		for {
			env, err := clientConn.Recv()
			if err != nil {
				return
			}
			req := env.GetOperationHookRequest()
			if req == nil {
				continue
			}
			resp := gcpc.NewOperationHookResponse(req.RequestId, map[string]string{"shared.ok": "1"})
			_ = clientConn.Send(resp)
		}
	}()

	reqID := NextRequestID()
	req := gcpc.NewOperationHookRequest(reqID, "op_1", "command", "", "start", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	respCh, err := pc.Send(ctx, req, reqID)
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	select {
	case env := <-respCh:
		elapsed := time.Since(start)
		if elapsed > 50*time.Millisecond {
			t.Errorf("response took %v, want <50ms — readLoop may not be routing OperationHookResponse", elapsed)
		}
		resp := env.GetOperationHookResponse()
		if resp == nil {
			t.Fatalf("expected OperationHookResponse payload, got %T", env.Payload)
		}
		if resp.RequestId != reqID {
			t.Errorf("request_id mismatch: got %q, want %q", resp.RequestId, reqID)
		}
		if resp.ContextValues["shared.ok"] != "1" {
			t.Errorf("context value not round-tripped: got %v", resp.ContextValues)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for OperationHookResponse — readLoop dropped the envelope")
	}
}
