package main

import (
	"context"
	"fmt"

	"gocache/api/events"
	gcpc "gocache/api/gcpc/v1"
	apiResp "gocache/commons/resp"
	"gocache/api/version"
	"gocache/sdk/pluginsdk"
)

const pluginName = "pubsub"

// Compile-time interface checks.
var (
	_ pluginsdk.CommandPlugin = (*PubSub)(nil)
	_ pluginsdk.HookPlugin   = (*PubSub)(nil)
	_ pluginsdk.EventPlugin  = (*PubSub)(nil)
	_ pluginsdk.ScopePlugin  = (*PubSub)(nil)
	_ pluginsdk.QueryPlugin  = (*PubSub)(nil)
)

type PubSub struct {
	manager *SubscriptionManager
	session *pluginsdk.Session
}

func (p *PubSub) SetSession(s *pluginsdk.Session) {
	p.session = s
}

func (p *PubSub) Name() string    { return pluginName }
func (p *PubSub) Version() string { return version.Version }
func (p *PubSub) Critical() bool  { return false }

func (p *PubSub) OnHealthCheck(context.Context) error { return nil }
func (p *PubSub) OnShutdown(context.Context) error    { return nil }

func (p *PubSub) Commands() []pluginsdk.CommandDecl {
	return []pluginsdk.CommandDecl{
		{Name: "SUBSCRIBE", MinArgs: 1, MaxArgs: -1, ReadOnly: true, KeyArgIndex: -1},
		{Name: "UNSUBSCRIBE", MinArgs: 0, MaxArgs: -1, ReadOnly: true, KeyArgIndex: -1},
		{Name: "PSUBSCRIBE", MinArgs: 1, MaxArgs: -1, ReadOnly: true, KeyArgIndex: -1},
		{Name: "PUNSUBSCRIBE", MinArgs: 0, MaxArgs: -1, ReadOnly: true, KeyArgIndex: -1},
		{Name: "PUBLISH", MinArgs: 2, MaxArgs: 2, ReadOnly: true, KeyArgIndex: -1},
	}
}

func (p *PubSub) HandleCommand(_ context.Context, cmd *gcpc.CommandInfoV1, conn *gcpc.ConnectionInfoV1, _ map[string]string) *pluginsdk.CommandResult {
	connID := conn.GetId()
	if connID == "" {
		return &pluginsdk.CommandResult{Value: fmt.Errorf("ERR no connection context")}
	}

	switch cmd.Name {
	case "SUBSCRIBE":
		return p.handleSubscribe(connID, cmd.Args)
	case "UNSUBSCRIBE":
		return p.handleUnsubscribe(connID, cmd.Args)
	case "PSUBSCRIBE":
		return p.handlePSubscribe(connID, cmd.Args)
	case "PUNSUBSCRIBE":
		return p.handlePUnsubscribe(connID, cmd.Args)
	case "PUBLISH":
		return p.handlePublish(connID, cmd.Args[0], cmd.Args[1])
	default:
		return &pluginsdk.CommandResult{Value: fmt.Errorf("ERR unknown command '%s'", cmd.Name)}
	}
}

func (p *PubSub) handleSubscribe(connID string, channels []string) *pluginsdk.CommandResult {
	for _, ch := range channels {
		p.manager.Subscribe(connID, ch)
		count := p.manager.SubscriptionCount(connID)
		msg := apiResp.EncodeArray(
			apiResp.EncodeBulkString("subscribe"),
			apiResp.EncodeBulkString(ch),
			apiResp.EncodeInteger(int64(count)),
		)
		_ = p.session.PushToClient(connID, msg)
	}
	return &pluginsdk.CommandResult{SuppressResponse: true}
}

func (p *PubSub) handleUnsubscribe(connID string, channels []string) *pluginsdk.CommandResult {
	if len(channels) == 0 {
		channels = p.manager.Channels(connID)
	}
	if len(channels) == 0 {
		msg := apiResp.EncodeArray(
			apiResp.EncodeBulkString("unsubscribe"),
			apiResp.EncodeNullBulk(),
			apiResp.EncodeInteger(0),
		)
		_ = p.session.PushToClient(connID, msg)
		return &pluginsdk.CommandResult{SuppressResponse: true}
	}
	for _, ch := range channels {
		p.manager.Unsubscribe(connID, ch)
		count := p.manager.SubscriptionCount(connID)
		msg := apiResp.EncodeArray(
			apiResp.EncodeBulkString("unsubscribe"),
			apiResp.EncodeBulkString(ch),
			apiResp.EncodeInteger(int64(count)),
		)
		_ = p.session.PushToClient(connID, msg)
	}
	return &pluginsdk.CommandResult{SuppressResponse: true}
}

func (p *PubSub) handlePSubscribe(connID string, patterns []string) *pluginsdk.CommandResult {
	for _, pat := range patterns {
		p.manager.PSubscribe(connID, pat)
		count := p.manager.SubscriptionCount(connID)
		msg := apiResp.EncodeArray(
			apiResp.EncodeBulkString("psubscribe"),
			apiResp.EncodeBulkString(pat),
			apiResp.EncodeInteger(int64(count)),
		)
		_ = p.session.PushToClient(connID, msg)
	}
	return &pluginsdk.CommandResult{SuppressResponse: true}
}

func (p *PubSub) handlePUnsubscribe(connID string, patterns []string) *pluginsdk.CommandResult {
	if len(patterns) == 0 {
		patterns = p.manager.Patterns(connID)
	}
	if len(patterns) == 0 {
		msg := apiResp.EncodeArray(
			apiResp.EncodeBulkString("punsubscribe"),
			apiResp.EncodeNullBulk(),
			apiResp.EncodeInteger(0),
		)
		_ = p.session.PushToClient(connID, msg)
		return &pluginsdk.CommandResult{SuppressResponse: true}
	}
	for _, pat := range patterns {
		p.manager.PUnsubscribe(connID, pat)
		count := p.manager.SubscriptionCount(connID)
		msg := apiResp.EncodeArray(
			apiResp.EncodeBulkString("punsubscribe"),
			apiResp.EncodeBulkString(pat),
			apiResp.EncodeInteger(int64(count)),
		)
		_ = p.session.PushToClient(connID, msg)
	}
	return &pluginsdk.CommandResult{SuppressResponse: true}
}

func (p *PubSub) handlePublish(connID, channel, message string) *pluginsdk.CommandResult {
	matches := p.manager.Publish(channel)

	// Pre-encode the channel message once for exact-match subscribers.
	// Pattern matches need per-pattern encoding so those are built inline.
	var channelMsg []byte
	hasChannel := false
	for _, m := range matches {
		if m.Pattern == "" {
			hasChannel = true
			break
		}
	}
	if hasChannel {
		b := make([]byte, 0, 64+len(channel)+len(message))
		b = apiResp.AppendArrayHeader(b, 3)
		b = apiResp.AppendBulkString(b, "message")
		b = apiResp.AppendBulkString(b, channel)
		b = apiResp.AppendBulkString(b, message)
		channelMsg = b
	}

	for _, m := range matches {
		if m.Pattern == "" {
			_ = p.session.PushToClient(m.ConnID, channelMsg)
		} else {
			b := make([]byte, 0, 64+len(m.Pattern)+len(channel)+len(message))
			b = apiResp.AppendArrayHeader(b, 4)
			b = apiResp.AppendBulkString(b, "pmessage")
			b = apiResp.AppendBulkString(b, m.Pattern)
			b = apiResp.AppendBulkString(b, channel)
			b = apiResp.AppendBulkString(b, message)
			_ = p.session.PushToClient(m.ConnID, b)
		}
	}

	return &pluginsdk.CommandResult{Value: len(matches)}
}

// Hooks returns the hook declarations for subscription mode enforcement.
func (p *PubSub) Hooks() []pluginsdk.HookDecl {
	return []pluginsdk.HookDecl{
		{Pattern: "*", Phase: pluginsdk.HookPhasePre, Blocking: true},
	}
}

// HandleHook enforces subscription mode: when a connection has active
// subscriptions, only subscription-related commands are allowed.
func (p *PubSub) HandleHook(_ context.Context, req *pluginsdk.HookRequest) *pluginsdk.HookResponse {
	if req.Phase != pluginsdk.HookPhasePre {
		return nil
	}
	connID := req.Connection.GetId()
	if connID == "" || !p.manager.IsSubscribed(connID) {
		return nil
	}
	switch req.Command.GetName() {
	case "SUBSCRIBE", "UNSUBSCRIBE", "PSUBSCRIBE", "PUNSUBSCRIBE", "PING", "RESET", "QUIT":
		return nil
	default:
		return &pluginsdk.HookResponse{
			Deny:       true,
			DenyReason: "ERR only (P)SUBSCRIBE / (P)UNSUBSCRIBE / PING / QUIT / RESET are allowed in subscribed state",
		}
	}
}

// EventTypes returns the event types this plugin subscribes to.
func (p *PubSub) EventTypes() []string {
	return []string{string(events.ConnectionClose)}
}

// HandleEvent cleans up subscriptions when a connection disconnects.
func (p *PubSub) HandleEvent(_ context.Context, evt *gcpc.EventV1) {
	if evt.Type == string(events.ConnectionClose) {
		if cc := evt.GetConnectionClose(); cc != nil && cc.ConnectionId != "" {
			p.manager.RemoveConnection(cc.ConnectionId)
		}
	}
}

// Scopes returns the permission scopes this plugin requires.
func (p *PubSub) Scopes() []string {
	return []string{"write", "hook:pre", "events"}
}

func main() {
	plugin := &PubSub{
		manager: NewSubscriptionManager(),
	}
	pluginsdk.Run(context.Background(), plugin)
}
