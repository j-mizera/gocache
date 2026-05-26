package server

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	apiEvents "gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	apiplugin "gocache/api/plugin"
	apiResp "gocache/api/resp"
	"gocache/api/transport"
	"gocache/pkg/blocking"
	"gocache/pkg/cache"
	"gocache/pkg/engine"
	serverEvents "gocache/pkg/events"
	serverOps "gocache/pkg/operations"
	"gocache/pkg/plugin/cmdhooks"
	pluginmgr "gocache/pkg/plugin/manager"
	"gocache/pkg/resp"
	"gocache/pkg/watch"
)

type pubsubState struct {
	mu           sync.Mutex
	channels     map[string]map[string]struct{} // channel → connID set
	connChannels map[string]map[string]struct{} // connID → channel set
}

func newPubsubState() *pubsubState {
	return &pubsubState{
		channels:     make(map[string]map[string]struct{}),
		connChannels: make(map[string]map[string]struct{}),
	}
}

func (s *pubsubState) subscribe(connID, channel string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.channels[channel] == nil {
		s.channels[channel] = make(map[string]struct{})
	}
	s.channels[channel][connID] = struct{}{}
	if s.connChannels[connID] == nil {
		s.connChannels[connID] = make(map[string]struct{})
	}
	s.connChannels[connID][channel] = struct{}{}
	return len(s.connChannels[connID])
}

func (s *pubsubState) unsubscribe(connID, channel string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if subs, ok := s.channels[channel]; ok {
		delete(subs, connID)
		if len(subs) == 0 {
			delete(s.channels, channel)
		}
	}
	if chs, ok := s.connChannels[connID]; ok {
		delete(chs, channel)
		if len(chs) == 0 {
			delete(s.connChannels, connID)
		}
	}
	if chs, ok := s.connChannels[connID]; ok {
		return len(chs)
	}
	return 0
}

func (s *pubsubState) channelsFor(connID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	chs := s.connChannels[connID]
	out := make([]string, 0, len(chs))
	for ch := range chs {
		out = append(out, ch)
	}
	return out
}

func (s *pubsubState) subscribers(channel string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := s.channels[channel]
	out := make([]string, 0, len(subs))
	for id := range subs {
		out = append(out, id)
	}
	return out
}

func (s *pubsubState) isSubscribed(connID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.connChannels[connID]) > 0
}

func (s *pubsubState) removeConnection(connID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.connChannels[connID] {
		if subs, ok := s.channels[ch]; ok {
			delete(subs, connID)
			if len(subs) == 0 {
				delete(s.channels, ch)
			}
		}
	}
	delete(s.connChannels, connID)
}

type simulatedPlugin struct {
	t     *testing.T
	conn  *transport.Conn
	state *pubsubState
}

func (p *simulatedPlugin) handleCommands(env *gcpc.EnvelopeV1) {
	req := env.GetCommandRequest()
	if req == nil {
		return
	}
	connID := req.Connection.GetId()

	switch req.Command.GetName() {
	case "SUBSCRIBE":
		for _, ch := range req.Command.Args {
			count := p.state.subscribe(connID, ch)
			msg := apiResp.EncodeArray(
				apiResp.EncodeBulkString("subscribe"),
				apiResp.EncodeBulkString(ch),
				apiResp.EncodeInteger(int64(count)),
			)
			p.conn.Send(gcpc.NewClientPush(connID, msg))
		}
		p.conn.Send(gcpc.NewCommandResponse(req.RequestId, gcpc.ResultFromInterface(nil), true))

	case "UNSUBSCRIBE":
		channels := req.Command.Args
		if len(channels) == 0 {
			channels = p.state.channelsFor(connID)
		}
		if len(channels) == 0 {
			msg := apiResp.EncodeArray(
				apiResp.EncodeBulkString("unsubscribe"),
				apiResp.EncodeNullBulk(),
				apiResp.EncodeInteger(0),
			)
			p.conn.Send(gcpc.NewClientPush(connID, msg))
		} else {
			for _, ch := range channels {
				count := p.state.unsubscribe(connID, ch)
				msg := apiResp.EncodeArray(
					apiResp.EncodeBulkString("unsubscribe"),
					apiResp.EncodeBulkString(ch),
					apiResp.EncodeInteger(int64(count)),
				)
				p.conn.Send(gcpc.NewClientPush(connID, msg))
			}
		}
		p.conn.Send(gcpc.NewCommandResponse(req.RequestId, gcpc.ResultFromInterface(nil), true))

	case "PUBLISH":
		if len(req.Command.Args) < 2 {
			p.conn.Send(gcpc.NewCommandResponse(req.RequestId, gcpc.ResultFromInterface("ERR wrong number of arguments"), false))
			return
		}
		channel, message := req.Command.Args[0], req.Command.Args[1]
		subs := p.state.subscribers(channel)
		for _, sub := range subs {
			b := make([]byte, 0, 64+len(channel)+len(message))
			b = apiResp.AppendArrayHeader(b, 3)
			b = apiResp.AppendBulkString(b, "message")
			b = apiResp.AppendBulkString(b, channel)
			b = apiResp.AppendBulkString(b, message)
			p.conn.Send(gcpc.NewClientPush(sub, b))
		}
		p.conn.Send(gcpc.NewCommandResponse(req.RequestId, gcpc.ResultFromInterface(strconv.Itoa(len(subs))), false))

	default:
		p.conn.Send(gcpc.NewCommandResponse(req.RequestId, gcpc.ResultFromInterface("ERR unknown command"), false))
	}
}

func (p *simulatedPlugin) handleHook(env *gcpc.EnvelopeV1) {
	req := env.GetHookRequest()
	if req == nil {
		return
	}
	connID := req.Connection.GetId()
	if connID != "" && p.state.isSubscribed(connID) {
		switch req.Command.GetName() {
		case "SUBSCRIBE", "UNSUBSCRIBE", "PSUBSCRIBE", "PUNSUBSCRIBE", "PING", "RESET", "QUIT":
		default:
			p.conn.Send(gcpc.NewHookResponse(req.RequestId, true,
				"ERR only (P)SUBSCRIBE / (P)UNSUBSCRIBE / PING / QUIT / RESET are allowed in subscribed state",
				nil))
			return
		}
	}
	p.conn.Send(gcpc.NewHookResponse(req.RequestId, false, "", nil))
}

func (p *simulatedPlugin) handleEvent(env *gcpc.EnvelopeV1) {
	evt := env.GetEvent()
	if evt == nil {
		return
	}
	if evt.Type == string(apiEvents.ConnectionClose) {
		if cc := evt.GetConnectionClose(); cc != nil && cc.ConnectionId != "" {
			p.state.removeConnection(cc.ConnectionId)
		}
	}
}

func (p *simulatedPlugin) loop(ctx context.Context) {
	for {
		env, err := p.conn.Recv()
		if err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		switch env.Payload.(type) {
		case *gcpc.EnvelopeV1_CommandRequest:
			p.handleCommands(env)
		case *gcpc.EnvelopeV1_HookRequest:
			p.handleHook(env)
		case *gcpc.EnvelopeV1_Event:
			p.handleEvent(env)
		}
	}
}

type pluginServerState struct {
	srv *Server
}

func (s *pluginServerState) IsShuttingDown() bool   { return false }
func (s *pluginServerState) StartTime() time.Time   { return time.Now() }
func (s *pluginServerState) ActiveConnections() int { return 0 }
func (s *pluginServerState) CacheKeys() int         { return 0 }
func (s *pluginServerState) CacheUsedBytes() int64  { return 0 }
func (s *pluginServerState) CacheMaxBytes() int64   { return 0 }

func startTestServerWithPubSub(t *testing.T) string {
	t.Helper()
	c := cache.New()
	e := engine.New(c)
	t.Cleanup(func() { e.Stop() })

	br := blocking.NewRegistry()
	wm := watch.NewManager()
	c.SetOnMutate(wm.NotifyMutation)
	c.SetOnMutateAll(wm.NotifyAll)

	srv := New("127.0.0.1:0", c, e, "", br, wm)
	tracker := serverOps.NewTracker()
	eventBus := serverEvents.NewBus()

	srv.SetEmitter(eventBus)
	srv.SetTracker(tracker)

	sockPath := t.TempDir() + "/pubsub.sock"
	pluginCfg := apiplugin.PluginsConfig{
		Enabled:         true,
		SocketPath:      sockPath,
		HealthInterval:  60 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		MaxRestarts:     0,
		ConnectTimeout:  5 * time.Second,
		Overrides: map[string]apiplugin.PluginOverride{
			"pubsub": {Scopes: []string{"read", "write", "hook:pre", "events"}},
		},
	}

	mgr := pluginmgr.NewManager(pluginCfg, srv.CoreCommandNames(), &pluginServerState{srv: srv})
	mgr.SetTracker(tracker)
	mgr.SetClientPusher(srv.ConnRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgr.TestSetCancel(cancel)
	mgr.TestAddInstance("pubsub")
	mgr.SetEventBus(eventBus)

	listener, err := transport.NewListener(sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go mgr.TestHandleConnection(ctx, conn)
		}
	}()

	state := newPubsubState()
	raw, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial plugin socket: %v", err)
	}
	tc := transport.NewConn(raw)
	t.Cleanup(func() { tc.Close() })

	reg := &gcpc.RegisterV1{
		Name:            "pubsub",
		Version:         "0.1.0-test",
		RequestedScopes: []string{"read", "write", "hook:pre", "events"},
		Commands: []*gcpc.CommandDeclV1{
			{Name: "SUBSCRIBE", MinArgs: 1, MaxArgs: -1, Readonly: true},
			{Name: "UNSUBSCRIBE", MinArgs: 0, MaxArgs: -1, Readonly: true},
			{Name: "PUBLISH", MinArgs: 2, MaxArgs: 2, Readonly: true},
		},
		Hooks: []*gcpc.HookDeclV1{
			{Pattern: "*", Phase: gcpc.HookPhaseV1_HOOK_PHASE_PRE, Blocking: true},
		},
	}
	regEnv := &gcpc.EnvelopeV1{
		Version: gcpc.ProtocolVersion,
		Payload: &gcpc.EnvelopeV1_Register{Register: reg},
	}
	if err := tc.Send(regEnv); err != nil {
		t.Fatalf("send register: %v", err)
	}
	ackEnv, err := tc.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	ack := ackEnv.GetRegisterAck()
	if ack == nil || !ack.Accepted {
		t.Fatalf("registration rejected: %v", ack)
	}

	tc.Send(gcpc.NewEventSubscribe([]string{string(apiEvents.ConnectionClose)}))

	sim := &simulatedPlugin{t: t, conn: tc, state: state}
	go sim.loop(ctx)

	// Wait for event subscription to be active.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eventBus.HasSubscriber("plugin:pubsub") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	srv.SetPluginRouter(mgr.Router())
	srv.SetHookExecutor(cmdhooks.NewExecutor(mgr.HookRegistry(), 5*time.Second))

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	srv.listener = tcpListener
	go srv.acceptConnections(ctx)
	t.Cleanup(func() { srv.Shutdown(2 * time.Second) })

	return tcpListener.Addr().String()
}

func writeCommand(t *testing.T, conn net.Conn, args ...string) {
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

var testReaders sync.Map

func readerFor(conn net.Conn) *resp.Reader {
	r, _ := testReaders.LoadOrStore(conn, resp.NewReader(conn))
	return r.(*resp.Reader)
}

func readValue(t *testing.T, conn net.Conn, timeout time.Duration) resp.Value {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	val, err := readerFor(conn).Read()
	if err != nil {
		t.Fatalf("readValue: %v", err)
	}
	return val
}

func assertPushArray(t *testing.T, v resp.Value, elements ...string) {
	t.Helper()
	if v.Type != resp.Array {
		t.Fatalf("expected array, got type=%c str=%q", v.Type, v.Str)
	}
	if len(v.Array) != len(elements) {
		t.Fatalf("expected %d elements, got %d", len(elements), len(v.Array))
	}
	for i, want := range elements {
		got := valueStr(v.Array[i])
		if got != want {
			t.Errorf("element[%d] = %q, want %q", i, got, want)
		}
	}
}

func valueStr(v resp.Value) string {
	switch v.Type {
	case resp.Integer:
		return fmt.Sprintf("%d", v.Integer)
	default:
		return v.Str
	}
}
