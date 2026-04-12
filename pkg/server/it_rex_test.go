package server

import (
	"context"
	"net"
	"sync"
	"testing"

	"gocache/pkg/command"
	"gocache/pkg/resp"
)

// fakeHookExecutor is an in-process command.HookExecutor that captures the
// hookCtx maps passed to RunPreHooks/RunPostHooks for inspection in tests.
type fakeHookExecutor struct {
	mu        sync.Mutex
	preCalls  []map[string]string
	postCalls []map[string]string
}

func (f *fakeHookExecutor) HasAny() bool { return true }

func (f *fakeHookExecutor) RunPreHooks(_ context.Context, _ string, _ []string, hookCtx map[string]string) *command.PreHookResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]string, len(hookCtx))
	for k, v := range hookCtx {
		cp[k] = v
	}
	f.preCalls = append(f.preCalls, cp)
	return &command.PreHookResult{Denied: false, Context: hookCtx}
}

func (f *fakeHookExecutor) RunPostHooks(_ context.Context, _ string, _ []string, _, _ string, hookCtx map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]string, len(hookCtx))
	for k, v := range hookCtx {
		cp[k] = v
	}
	f.postCalls = append(f.postCalls, cp)
}

func (f *fakeHookExecutor) lastPreCall() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.preCalls) == 0 {
		return nil
	}
	return f.preCalls[len(f.preCalls)-1]
}

// writeCmd writes a RESP array to the connection without reading a response.
// Callers are responsible for calling readResp when a response is expected.
func writeCmd(t *testing.T, conn net.Conn, args ...string) {
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
}

// readResp reads a single RESP value from the connection.
func readResp(t *testing.T, conn net.Conn) resp.Value {
	t.Helper()
	r := resp.NewReader(conn)
	val, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return val
}

// setupREXCapture registers a TEST.CAPTURE command on the server's evaluator
// that records the CmdMeta attached to the command. Returns a function that
// returns the most recently captured metadata snapshot.
func setupREXCapture(t *testing.T, srv *Server) func() map[string]string {
	t.Helper()
	var mu sync.Mutex
	var captured map[string]string

	srv.evaluator.RegisterHandler("TEST.CAPTURE", func(cmdCtx *command.Context) command.Result {
		mu.Lock()
		defer mu.Unlock()
		if cmdCtx.Client.CmdMeta != nil {
			captured = make(map[string]string, len(cmdCtx.Client.CmdMeta))
			for k, v := range cmdCtx.Client.CmdMeta {
				captured[k] = v
			}
		} else {
			captured = nil
		}
		return command.Result{Value: "OK"}
	})

	return func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		if captured == nil {
			return nil
		}
		cp := make(map[string]string, len(captured))
		for k, v := range captured {
			cp[k] = v
		}
		return cp
	}
}

func TestServer_REX_META_LinesBeforeCommand(t *testing.T) {
	srv, addr := startTestServer(t, "")
	getCaptured := setupREXCapture(t, srv)

	conn := dial(t, addr)
	defer conn.Close()

	// Negotiate REXV 1.
	helloRes := sendCommand(t, conn, "HELLO", "3", "REXV", "1")
	if helloRes.Type == resp.Error {
		t.Fatalf("HELLO REXV 1 failed: %s", helloRes.Str)
	}

	t.Run("single META line before command", func(t *testing.T) {
		// Send META line, expect +OK response (RESP-compliant).
		writeCmd(t, conn, "META", "traceparent", "00-abc123")
		assertOK(t, readResp(t, conn))

		// Send command, expect +OK response.
		writeCmd(t, conn, "TEST.CAPTURE")
		assertOK(t, readResp(t, conn))

		meta := getCaptured()
		if meta == nil {
			t.Fatal("CmdMeta was nil; expected metadata to be attached")
		}
		if meta["traceparent"] != "00-abc123" {
			t.Errorf("traceparent=%q, want 00-abc123", meta["traceparent"])
		}
	})

	t.Run("multiple META lines accumulate", func(t *testing.T) {
		writeCmd(t, conn, "META", "traceparent", "00-xyz")
		assertOK(t, readResp(t, conn))
		writeCmd(t, conn, "META", "auth.jwt", "eyJhbG")
		assertOK(t, readResp(t, conn))
		writeCmd(t, conn, "META", "tenant.id", "team-a")
		assertOK(t, readResp(t, conn))
		writeCmd(t, conn, "TEST.CAPTURE")
		assertOK(t, readResp(t, conn))

		meta := getCaptured()
		if len(meta) != 3 {
			t.Fatalf("expected 3 META entries, got %d: %v", len(meta), meta)
		}
		if meta["traceparent"] != "00-xyz" {
			t.Errorf("traceparent=%q", meta["traceparent"])
		}
		if meta["auth.jwt"] != "eyJhbG" {
			t.Errorf("auth.jwt=%q", meta["auth.jwt"])
		}
		if meta["tenant.id"] != "team-a" {
			t.Errorf("tenant.id=%q", meta["tenant.id"])
		}
	})

	t.Run("META value with spaces joined", func(t *testing.T) {
		writeCmd(t, conn, "META", "authorization", "Bearer", "eyJhbG...")
		assertOK(t, readResp(t, conn))
		writeCmd(t, conn, "TEST.CAPTURE")
		assertOK(t, readResp(t, conn))

		meta := getCaptured()
		if meta["authorization"] != "Bearer eyJhbG..." {
			t.Errorf("authorization=%q, want %q", meta["authorization"], "Bearer eyJhbG...")
		}
	})

	t.Run("command without META has nil CmdMeta", func(t *testing.T) {
		writeCmd(t, conn, "TEST.CAPTURE")
		assertOK(t, readResp(t, conn))

		meta := getCaptured()
		if meta != nil {
			t.Errorf("expected nil CmdMeta, got %v", meta)
		}
	})

	t.Run("META cleared after command consumes it", func(t *testing.T) {
		writeCmd(t, conn, "META", "once", "value1")
		assertOK(t, readResp(t, conn))
		writeCmd(t, conn, "TEST.CAPTURE")
		assertOK(t, readResp(t, conn))
		meta1 := getCaptured()
		if meta1["once"] != "value1" {
			t.Fatalf("first capture: %v", meta1)
		}

		writeCmd(t, conn, "TEST.CAPTURE")
		assertOK(t, readResp(t, conn))
		meta2 := getCaptured()
		if meta2 != nil {
			t.Errorf("second command should have nil CmdMeta, got %v", meta2)
		}
	})

	t.Run("META invalid key returns error and clears accumulator", func(t *testing.T) {
		writeCmd(t, conn, "META", "_reserved", "value")
		res := readResp(t, conn)
		if res.Type != resp.Error {
			t.Errorf("expected error for reserved key META, got type=%c str=%q", res.Type, res.Str)
		}

		writeCmd(t, conn, "TEST.CAPTURE")
		assertOK(t, readResp(t, conn))
		if meta := getCaptured(); meta != nil {
			t.Errorf("expected nil CmdMeta after error, got %v", meta)
		}
	})
}

func TestServer_REX_META_WithoutREXV(t *testing.T) {
	// Without HELLO REXV 1, META lines should be treated as regular (unknown) commands.
	srv, addr := startTestServer(t, "")
	_ = setupREXCapture(t, srv)

	conn := dial(t, addr)
	defer conn.Close()

	// Don't negotiate REXV. Send META as if it were a command.
	writeCmd(t, conn, "META", "traceparent", "abc")
	res := readResp(t, conn)
	// Should get an "unknown command" error since META isn't a real command
	// and REXV wasn't negotiated.
	if res.Type != resp.Error {
		t.Errorf("expected error for META without REXV, got type=%c str=%q", res.Type, res.Str)
	}
}

func TestServer_REXMeta_TransactionBypass(t *testing.T) {
	// REX.META is connection state (like AUTH/HELLO) and must execute
	// immediately inside MULTI rather than being queued. Verify the
	// command runs and the value is readable mid-transaction.
	_, addr := startTestServer(t, "")
	conn := dial(t, addr)
	defer conn.Close()

	sendCommand(t, conn, "HELLO", "3")

	// Start transaction
	assertOK(t, sendCommand(t, conn, "MULTI"))

	// REX.META SET inside MULTI must NOT return QUEUED -- it bypasses.
	res := sendCommand(t, conn, "REX.META", "SET", "auth.jwt", "in-tx-token")
	if res.Str == "QUEUED" {
		t.Fatal("REX.META SET inside MULTI should bypass the queue, got QUEUED")
	}
	assertOK(t, res)

	// REX.META GET should also bypass and return the value.
	res = sendCommand(t, conn, "REX.META", "GET", "auth.jwt")
	if res.Str == "QUEUED" {
		t.Fatal("REX.META GET inside MULTI should bypass the queue, got QUEUED")
	}
	assertBulk(t, res, "in-tx-token")

	// Regular commands inside MULTI should still queue.
	res = sendCommand(t, conn, "SET", "key", "value")
	assertQueued(t, res)

	// EXEC -- should only return result for the SET (REX.META wasn't queued).
	res = sendCommand(t, conn, "EXEC")
	results, ok := res.Array, res.Type == resp.Array
	if !ok {
		t.Fatalf("EXEC: expected array, got type=%c", res.Type)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 batched result (SET only), got %d", len(results))
	}
}

func TestServer_REXMeta_PrecedenceMetaOverridesConnDefaults(t *testing.T) {
	// Verify that per-command META overrides REX.META connection defaults
	// at the server boundary (CmdMeta vs RexMeta on ClientContext).
	srv, addr := startTestServer(t, "")
	getCaptured := setupREXCapture(t, srv)

	// Extend the test capture handler to also report the connection store contents,
	// since the precedence test needs to see both layers.
	// The handler runs in the server's connection goroutine; guard the shared
	// state with a mutex so the race detector has an explicit happens-before.
	var (
		connMu       sync.Mutex
		connDefaults map[string]string
	)
	srv.evaluator.RegisterHandler("TEST.CAPTURE_BOTH", func(cmdCtx *command.Context) command.Result {
		connMu.Lock()
		defer connMu.Unlock()
		connDefaults = nil
		if cmdCtx.Client.RexMeta != nil {
			connDefaults = cmdCtx.Client.RexMeta.All()
		}
		return command.Result{Value: "OK"}
	})
	readConnDefaults := func() map[string]string {
		connMu.Lock()
		defer connMu.Unlock()
		if connDefaults == nil {
			return nil
		}
		cp := make(map[string]string, len(connDefaults))
		for k, v := range connDefaults {
			cp[k] = v
		}
		return cp
	}

	conn := dial(t, addr)
	defer conn.Close()

	sendCommand(t, conn, "HELLO", "3", "REXV", "1")

	// Connection-scoped default
	assertOK(t, sendCommand(t, conn, "REX.META", "SET", "auth.jwt", "default-token"))

	// Per-command override
	writeCmd(t, conn, "META", "auth.jwt", "override-token")
	assertOK(t, readResp(t, conn))

	// Capture both layers
	writeCmd(t, conn, "TEST.CAPTURE_BOTH")
	assertOK(t, readResp(t, conn))

	gotConn := readConnDefaults()
	if gotConn["auth.jwt"] != "default-token" {
		t.Errorf("connection default: got %q, want default-token", gotConn["auth.jwt"])
	}

	// CmdMeta is consumed by the time TEST.CAPTURE_BOTH runs (the handler
	// reads it through cmdCtx.Client.CmdMeta which the server set before Eval).
	// Use the existing TEST.CAPTURE handler to read CmdMeta on a fresh command.
	_ = getCaptured

	// Send the same META + capture command to verify CmdMeta has the override.
	writeCmd(t, conn, "META", "auth.jwt", "override-token")
	assertOK(t, readResp(t, conn))
	writeCmd(t, conn, "TEST.CAPTURE")
	assertOK(t, readResp(t, conn))
	cmdMeta := getCaptured()
	if cmdMeta["auth.jwt"] != "override-token" {
		t.Errorf("CmdMeta: got %q, want override-token", cmdMeta["auth.jwt"])
	}

	// Sanity: connection default still intact (not mutated by per-command META).
	res := sendCommand(t, conn, "REX.META", "GET", "auth.jwt")
	assertBulk(t, res, "default-token")
}

func TestServer_REX_HookContextEndToEnd(t *testing.T) {
	// End-to-end: verify REX metadata reaches the hook executor under
	// the "shared.rex." prefix, with per-command META overriding REX.META
	// connection defaults at the merged hookCtx level.
	srv, addr := startTestServer(t, "")

	fake := &fakeHookExecutor{}
	srv.SetHookExecutor(fake)

	conn := dial(t, addr)
	defer conn.Close()

	sendCommand(t, conn, "HELLO", "3", "REXV", "1")

	// Set a connection-scoped default.
	assertOK(t, sendCommand(t, conn, "REX.META", "SET", "auth.jwt", "default-token"))
	assertOK(t, sendCommand(t, conn, "REX.META", "SET", "tenant.id", "team-a"))

	// Per-command override (auth.jwt only) + new key (traceparent).
	writeCmd(t, conn, "META", "auth.jwt", "override-token")
	assertOK(t, readResp(t, conn))
	writeCmd(t, conn, "META", "traceparent", "00-abc-def-01")
	assertOK(t, readResp(t, conn))

	// Run a real command -- this triggers hook context construction.
	writeCmd(t, conn, "SET", "k", "v")
	assertOK(t, readResp(t, conn))

	hookCtx := fake.lastPreCall()
	if hookCtx == nil {
		t.Fatal("pre-hook not called")
	}

	// Server-injected key always present.
	if _, ok := hookCtx[command.StartNs]; !ok {
		t.Errorf("missing _start_ns in hook context: %v", hookCtx)
	}

	// Per-command override should win.
	if got := hookCtx["shared.rex.auth.jwt"]; got != "override-token" {
		t.Errorf("auth.jwt: got %q, want override-token (per-command should win)", got)
	}

	// New per-command key should be present.
	if got := hookCtx["shared.rex.traceparent"]; got != "00-abc-def-01" {
		t.Errorf("traceparent: got %q, want 00-abc-def-01", got)
	}

	// Connection default not overridden by per-command should still be present.
	if got := hookCtx["shared.rex.tenant.id"]; got != "team-a" {
		t.Errorf("tenant.id: got %q, want team-a (connection default)", got)
	}

	// A subsequent command without META should only see connection defaults.
	writeCmd(t, conn, "SET", "k2", "v2")
	assertOK(t, readResp(t, conn))

	hookCtx2 := fake.lastPreCall()
	if got := hookCtx2["shared.rex.auth.jwt"]; got != "default-token" {
		t.Errorf("after override consumed: auth.jwt=%q, want default-token", got)
	}
	if _, hasTrace := hookCtx2["shared.rex.traceparent"]; hasTrace {
		t.Errorf("traceparent should be cleared on second command, got %q", hookCtx2["shared.rex.traceparent"])
	}
}

func TestServer_REXMeta_StickyConnection(t *testing.T) {
	// REX.META command sets connection-scoped defaults. Verify they appear
	// in the evaluator's hook-context injection path by reading via REX.META GET.
	_, addr := startTestServer(t, "")

	conn := dial(t, addr)
	defer conn.Close()

	// HELLO (REXV not required for REX.META standalone command)
	sendCommand(t, conn, "HELLO", "3")

	// SET
	res := sendCommand(t, conn, "REX.META", "SET", "auth.jwt", "my-token")
	assertOK(t, res)

	// GET
	res = sendCommand(t, conn, "REX.META", "GET", "auth.jwt")
	assertBulk(t, res, "my-token")

	// MSET
	res = sendCommand(t, conn, "REX.META", "MSET", "tenant.id", "team-a", "app.version", "1.0")
	assertOK(t, res)

	// GET the MSET values
	res = sendCommand(t, conn, "REX.META", "GET", "tenant.id")
	assertBulk(t, res, "team-a")

	// DEL
	res = sendCommand(t, conn, "REX.META", "DEL", "auth.jwt")
	assertInt(t, res, 1)

	// DEL again -- already gone
	res = sendCommand(t, conn, "REX.META", "DEL", "auth.jwt")
	assertInt(t, res, 0)
}
