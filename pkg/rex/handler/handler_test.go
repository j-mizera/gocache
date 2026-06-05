package handler_test

import (
	"errors"
	"reflect"
	"testing"

	apiobs "gocache/api/observability"
	commonobs "gocache/commons/observability"
	"gocache/commons/resp"
	"gocache/pkg/clientctx"
	"gocache/pkg/command"
	"gocache/pkg/rex"
	rexhandler "gocache/pkg/rex/handler"
)

func makeCtx(args ...string) *command.Context {
	return &command.Context{
		Client: clientctx.New(),
		Op:     "REX.META",
		Args:   args,
	}
}

func TestHandleRexMeta_Set(t *testing.T) {
	ctx := makeCtx("SET", "auth.jwt", "token123")
	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != "OK" {
		t.Fatalf("SET: expected OK, got %v", res.Value)
	}
	if ctx.Client.RexMeta == nil {
		t.Fatal("RexMeta store not lazily initialized")
	}
	v, ok := ctx.Client.RexMeta.Get("auth.jwt")
	if !ok || v != "token123" {
		t.Errorf("store got %q, %v; want token123, true", v, ok)
	}
}

func TestHandleRexMeta_SetValueWithSpaces(t *testing.T) {
	ctx := makeCtx("SET", "authorization", "Bearer", "eyJhbG...")
	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != "OK" {
		t.Fatalf("SET: %v", res.Value)
	}
	v, _ := ctx.Client.RexMeta.Get("authorization")
	if v != "Bearer eyJhbG..." {
		t.Errorf("value=%q, want %q", v, "Bearer eyJhbG...")
	}
}

func TestHandleRexMeta_SetReservedKeyFails(t *testing.T) {
	ctx := makeCtx("SET", "_reserved", "val")
	res := rexhandler.HandleRexMeta(ctx)
	if !errors.Is(res.Err, rex.ErrReservedPrefix) {
		t.Errorf("expected ErrReservedPrefix for reserved key, got %v (value=%v)", res.Err, res.Value)
	}
}

func TestHandleRexMeta_SetMissingArgs(t *testing.T) {
	ctx := makeCtx("SET", "onlykey")
	res := rexhandler.HandleRexMeta(ctx)
	if v, ok := res.Value.(resp.Value); !ok || v.Type != resp.Error {
		t.Errorf("expected error, got %v", res.Value)
	}
}

func TestHandleRexMeta_SetAdvancesConnectionBaseOnly(t *testing.T) {
	ctx := makeCtx("SET", "tenant", "acme")
	ctx.Client.ConnectionIdentity = apiobs.ConnectionIdentity(41)
	manager := newRexTelemetryManager()
	manager.UpdateConnectionContext(ctx.Client.ConnectionIdentity, []byte("tenant"), []byte("old"), []byte("role"), []byte("reader"))
	ctx.SetOperationTrackerManager(manager)

	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != "OK" {
		t.Fatalf("SET: expected OK, got %v (err=%v)", res.Value, res.Err)
	}

	current := currentConnectionContext(t, manager, ctx.Client.ConnectionIdentity)
	if current["tenant"] != "acme" || current["role"] != "reader" {
		t.Fatalf("current connection context = %+v, want tenant=acme role=reader", current)
	}
}

func TestHandleRexMeta_MSet(t *testing.T) {
	ctx := makeCtx("MSET", "auth.jwt", "tok1", "tenant.id", "team-a")
	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != "OK" {
		t.Fatalf("MSET: %v", res.Value)
	}
	if v, _ := ctx.Client.RexMeta.Get("auth.jwt"); v != "tok1" {
		t.Errorf("auth.jwt=%q, want tok1", v)
	}
	if v, _ := ctx.Client.RexMeta.Get("tenant.id"); v != "team-a" {
		t.Errorf("tenant.id=%q, want team-a", v)
	}
}

func TestHandleRexMeta_MSetAdvancesConnectionBaseOnly(t *testing.T) {
	ctx := makeCtx("MSET", "auth.jwt", "tok1", "tenant.id", "team-a")
	ctx.Client.ConnectionIdentity = apiobs.ConnectionIdentity(42)
	manager := newRexTelemetryManager()
	ctx.SetOperationTrackerManager(manager)

	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != "OK" {
		t.Fatalf("MSET: expected OK, got %v (err=%v)", res.Value, res.Err)
	}

	want := map[string]string{"auth.jwt": "tok1", "tenant.id": "team-a"}
	current := currentConnectionContext(t, manager, ctx.Client.ConnectionIdentity)
	if !reflect.DeepEqual(current, want) {
		t.Fatalf("current connection context = %+v, want %+v", current, want)
	}
}

func TestHandleRexMeta_MSetOddArgs(t *testing.T) {
	ctx := makeCtx("MSET", "k1", "v1", "k2")
	res := rexhandler.HandleRexMeta(ctx)
	if v, ok := res.Value.(resp.Value); !ok || v.Type != resp.Error {
		t.Errorf("expected error for odd args, got %v", res.Value)
	}
}

func TestHandleRexMeta_Get(t *testing.T) {
	ctx := makeCtx("SET", "auth.jwt", "token123")
	rexhandler.HandleRexMeta(ctx)

	ctx.Args = []string{"GET", "auth.jwt"}
	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != "token123" {
		t.Errorf("GET: got %v, want token123", res.Value)
	}
}

func TestHandleRexMeta_GetMissing(t *testing.T) {
	ctx := makeCtx("GET", "nope")
	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != nil {
		t.Errorf("GET missing: got %v, want nil", res.Value)
	}
}

func TestHandleRexMeta_GetEmptyStore(t *testing.T) {
	ctx := makeCtx("GET", "anything")
	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != nil {
		t.Errorf("GET on nil store: got %v, want nil", res.Value)
	}
}

func TestHandleRexMeta_Del(t *testing.T) {
	ctx := makeCtx("SET", "auth.jwt", "tok")
	rexhandler.HandleRexMeta(ctx)

	ctx.Args = []string{"DEL", "auth.jwt"}
	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != 1 {
		t.Errorf("DEL existing: got %v, want 1", res.Value)
	}

	res = rexhandler.HandleRexMeta(ctx)
	if res.Value != 0 {
		t.Errorf("DEL missing: got %v, want 0", res.Value)
	}
}

func TestHandleRexMeta_DelAdvancesConnectionBaseOnly(t *testing.T) {
	ctx := makeCtx("DEL", "role")
	ctx.Client.ConnectionIdentity = apiobs.ConnectionIdentity(43)
	ctx.Client.RexMeta = rex.NewStore()
	if err := ctx.Client.RexMeta.Set("role", "reader"); err != nil {
		t.Fatalf("seed rex store: %v", err)
	}
	manager := newRexTelemetryManager()
	manager.UpdateConnectionContext(ctx.Client.ConnectionIdentity, []byte("tenant"), []byte("acme"), []byte("role"), []byte("reader"))
	ctx.SetOperationTrackerManager(manager)

	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != 1 {
		t.Fatalf("DEL existing: got %v, want 1 (err=%v)", res.Value, res.Err)
	}

	current := currentConnectionContext(t, manager, ctx.Client.ConnectionIdentity)
	if current["tenant"] != "acme" {
		t.Fatalf("current connection context = %+v, want tenant=acme", current)
	}
	if _, ok := current["role"]; ok {
		t.Fatalf("current connection context should not contain removed role: %+v", current)
	}
}

func TestHandleRexMeta_DelEmptyStore(t *testing.T) {
	ctx := makeCtx("DEL", "nope")
	res := rexhandler.HandleRexMeta(ctx)
	if res.Value != 0 {
		t.Errorf("DEL on nil store: got %v, want 0", res.Value)
	}
}

func TestHandleRexMeta_List(t *testing.T) {
	ctx := makeCtx("MSET", "auth.jwt", "tok", "tenant.id", "team-a")
	rexhandler.HandleRexMeta(ctx)

	ctx.Args = []string{"LIST"}
	res := rexhandler.HandleRexMeta(ctx)
	m, ok := res.Value.(map[string]string)
	if !ok {
		t.Fatalf("LIST: expected map[string]string, got %T", res.Value)
	}
	if len(m) != 2 || m["auth.jwt"] != "tok" || m["tenant.id"] != "team-a" {
		t.Errorf("LIST: got %v", m)
	}
}

func TestHandleRexMeta_ListEmpty(t *testing.T) {
	ctx := makeCtx("LIST")
	res := rexhandler.HandleRexMeta(ctx)
	m, ok := res.Value.(map[string]string)
	if !ok {
		t.Fatalf("LIST: expected map, got %T", res.Value)
	}
	if len(m) != 0 {
		t.Errorf("LIST empty: got %v, want empty map", m)
	}
}

func TestHandleRexMeta_UnknownSubcommand(t *testing.T) {
	ctx := makeCtx("NOPE")
	res := rexhandler.HandleRexMeta(ctx)
	if v, ok := res.Value.(resp.Value); !ok || v.Type != resp.Error {
		t.Errorf("expected error for unknown subcommand, got %v", res.Value)
	}
}

func newRexTelemetryManager() *commonobs.SlotOperationTrackerManager {
	return commonobs.NewSlotOperationTrackerManager(commonobs.SlotTrackerConfig{
		ShardCount:            1,
		MinSegmentsPerShard:   1,
		MaxSegmentsPerShard:   1,
		SegmentSize:           1,
		RecordsPerOperation:   4,
		CompletedRingPerShard: 1,
	})
}

func currentConnectionContext(t *testing.T, manager *commonobs.SlotOperationTrackerManager, connection apiobs.ConnectionIdentity) map[string]string {
	t.Helper()
	version := manager.PinCurrentConnectionContextVersion(connection)
	if version.IsZero() {
		t.Fatal("current connection context version should be non-zero")
	}
	defer manager.ReleaseConnectionContextVersion(version)
	out := make(map[string]string)
	if !manager.VisitConnectionContextVersion(version, func(key, value string) bool {
		out[key] = value
		return true
	}) {
		t.Fatalf("current connection context version %d should be visitable", version)
	}
	return out
}
