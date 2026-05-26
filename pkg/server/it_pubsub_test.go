package server

import (
	"net"
	"testing"
	"time"

	"gocache/pkg/resp"
)

func TestIT_PubSub_SubscribeAndPublish(t *testing.T) {
	addr := startTestServerWithPubSub(t)

	sub := dial(t, addr)
	defer sub.Close()
	pub := dial(t, addr)
	defer pub.Close()

	writeCommand(t, sub, "SUBSCRIBE", "ch1")
	v := readValue(t, sub, 2*time.Second)
	assertPushArray(t, v, "subscribe", "ch1", "1")

	res := sendCommand(t, pub, "PUBLISH", "ch1", "hello")
	assertBulk(t, res, "1")

	msg := readValue(t, sub, 2*time.Second)
	assertPushArray(t, msg, "message", "ch1", "hello")
}

func TestIT_PubSub_MultipleChannels(t *testing.T) {
	addr := startTestServerWithPubSub(t)

	sub := dial(t, addr)
	defer sub.Close()
	pub := dial(t, addr)
	defer pub.Close()

	channels := []string{"news", "sports", "tech"}
	for i, ch := range channels {
		writeCommand(t, sub, "SUBSCRIBE", ch)
		v := readValue(t, sub, 2*time.Second)
		assertPushArray(t, v, "subscribe", ch, intToStr(i+1))
	}

	res := sendCommand(t, pub, "PUBLISH", "sports", "goal!")
	assertBulk(t, res, "1")

	msg := readValue(t, sub, 2*time.Second)
	assertPushArray(t, msg, "message", "sports", "goal!")
}

func TestIT_PubSub_Unsubscribe(t *testing.T) {
	addr := startTestServerWithPubSub(t)

	sub := dial(t, addr)
	defer sub.Close()
	pub := dial(t, addr)
	defer pub.Close()

	writeCommand(t, sub, "SUBSCRIBE", "a", "b")
	readValue(t, sub, 2*time.Second) // subscribe a
	readValue(t, sub, 2*time.Second) // subscribe b

	writeCommand(t, sub, "UNSUBSCRIBE", "a")
	v := readValue(t, sub, 2*time.Second)
	assertPushArray(t, v, "unsubscribe", "a", "1")

	res := sendCommand(t, pub, "PUBLISH", "a", "gone")
	assertBulk(t, res, "0")

	res = sendCommand(t, pub, "PUBLISH", "b", "still here")
	assertBulk(t, res, "1")

	msg := readValue(t, sub, 2*time.Second)
	assertPushArray(t, msg, "message", "b", "still here")
}

func TestIT_PubSub_UnsubscribeAll(t *testing.T) {
	addr := startTestServerWithPubSub(t)

	sub := dial(t, addr)
	defer sub.Close()
	pub := dial(t, addr)
	defer pub.Close()

	writeCommand(t, sub, "SUBSCRIBE", "x", "y")
	readValue(t, sub, 2*time.Second)
	readValue(t, sub, 2*time.Second)

	writeCommand(t, sub, "UNSUBSCRIBE")
	// Should get two unsubscribe confirmations (one per channel).
	readValue(t, sub, 2*time.Second)
	readValue(t, sub, 2*time.Second)

	res := sendCommand(t, pub, "PUBLISH", "x", "nope")
	assertBulk(t, res, "0")
}

func TestIT_PubSub_SubscriptionModeEnforcement(t *testing.T) {
	addr := startTestServerWithPubSub(t)

	conn := dial(t, addr)
	defer conn.Close()

	writeCommand(t, conn, "SUBSCRIBE", "ch")
	readValue(t, conn, 2*time.Second) // subscribe confirmation

	// SET should be denied in subscribed state.
	writeCommand(t, conn, "SET", "key", "val")
	errVal := readValue(t, conn, 2*time.Second)
	if errVal.Type != resp.Error {
		t.Fatalf("expected error for SET in subscribed state, got type=%c str=%q", errVal.Type, errVal.Str)
	}

	// PING should still work.
	writeCommand(t, conn, "PING")
	pong := readValue(t, conn, 2*time.Second)
	if pong.Str != "PONG" {
		t.Errorf("expected PONG, got %q", pong.Str)
	}

	// Unsubscribe and SET should work again.
	writeCommand(t, conn, "UNSUBSCRIBE", "ch")
	readValue(t, conn, 2*time.Second) // unsubscribe confirmation

	res := sendCommand(t, conn, "SET", "key", "val")
	assertOK(t, res)
}

func TestIT_PubSub_DisconnectCleanup(t *testing.T) {
	addr := startTestServerWithPubSub(t)

	sub := dial(t, addr)
	pub := dial(t, addr)
	defer pub.Close()

	writeCommand(t, sub, "SUBSCRIBE", "events")
	readValue(t, sub, 2*time.Second)

	res := sendCommand(t, pub, "PUBLISH", "events", "before")
	assertBulk(t, res, "1")
	readValue(t, sub, 2*time.Second) // consume the message

	sub.Close()

	// Give the server time to process the disconnect and propagate
	// the connection.close event to the plugin.
	time.Sleep(200 * time.Millisecond)

	res = sendCommand(t, pub, "PUBLISH", "events", "after")
	assertBulk(t, res, "0")
}

func TestIT_PubSub_MultipleSubscribers(t *testing.T) {
	addr := startTestServerWithPubSub(t)

	pub := dial(t, addr)
	defer pub.Close()

	conns := make([]net.Conn, 3)
	for i := 0; i < 3; i++ {
		c := dial(t, addr)
		conns[i] = c
		defer c.Close()

		writeCommand(t, c, "SUBSCRIBE", "broadcast")
		v := readValue(t, c, 2*time.Second)
		assertPushArray(t, v, "subscribe", "broadcast", "1")
	}

	res := sendCommand(t, pub, "PUBLISH", "broadcast", "hello all")
	assertBulk(t, res, "3")

	for i, c := range conns {
		msg := readValue(t, c, 2*time.Second)
		if msg.Type != resp.Array || len(msg.Array) < 3 {
			t.Fatalf("subscriber %d: expected message array, got type=%c", i, msg.Type)
		}
		if msg.Array[2].Str != "hello all" {
			t.Errorf("subscriber %d: got %q, want %q", i, msg.Array[2].Str, "hello all")
		}
	}
}

func TestIT_PubSub_PublishNoSubscribers(t *testing.T) {
	addr := startTestServerWithPubSub(t)

	conn := dial(t, addr)
	defer conn.Close()

	res := sendCommand(t, conn, "PUBLISH", "empty", "nobody home")
	assertBulk(t, res, "0")
}

func intToStr(n int) string {
	return string(rune('0' + n))
}
