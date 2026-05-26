package main

import (
	"sort"
	"testing"
)

func TestSubscribeUnsubscribe(t *testing.T) {
	m := NewSubscriptionManager()

	m.Subscribe("conn1", "news")
	m.Subscribe("conn1", "sports")
	m.Subscribe("conn2", "news")

	if !m.IsSubscribed("conn1") {
		t.Error("conn1 should be subscribed")
	}
	if m.SubscriptionCount("conn1") != 2 {
		t.Errorf("conn1 count = %d, want 2", m.SubscriptionCount("conn1"))
	}

	channels := m.Channels("conn1")
	sort.Strings(channels)
	if len(channels) != 2 || channels[0] != "news" || channels[1] != "sports" {
		t.Errorf("conn1 channels = %v, want [news sports]", channels)
	}

	m.Unsubscribe("conn1", "news")
	if m.SubscriptionCount("conn1") != 1 {
		t.Errorf("conn1 count after unsub = %d, want 1", m.SubscriptionCount("conn1"))
	}

	m.Unsubscribe("conn1", "sports")
	if m.IsSubscribed("conn1") {
		t.Error("conn1 should not be subscribed after full unsub")
	}
}

func TestPSubscribe(t *testing.T) {
	m := NewSubscriptionManager()

	m.PSubscribe("conn1", "news.*")
	m.Subscribe("conn2", "news.art")

	matches := m.Publish("news.art")
	if len(matches) != 2 {
		t.Fatalf("publish matches = %d, want 2", len(matches))
	}

	var channelMatch, patternMatch bool
	for _, match := range matches {
		if match.ConnID == "conn2" && match.Pattern == "" {
			channelMatch = true
		}
		if match.ConnID == "conn1" && match.Pattern == "news.*" {
			patternMatch = true
		}
	}
	if !channelMatch {
		t.Error("missing channel match for conn2")
	}
	if !patternMatch {
		t.Error("missing pattern match for conn1")
	}
}

func TestPublishNoSubscribers(t *testing.T) {
	m := NewSubscriptionManager()
	matches := m.Publish("empty")
	if len(matches) != 0 {
		t.Errorf("publish to empty channel = %d matches, want 0", len(matches))
	}
}

func TestRemoveConnection(t *testing.T) {
	m := NewSubscriptionManager()

	m.Subscribe("conn1", "a")
	m.Subscribe("conn1", "b")
	m.PSubscribe("conn1", "c.*")
	m.Subscribe("conn2", "a")

	m.RemoveConnection("conn1")

	if m.IsSubscribed("conn1") {
		t.Error("conn1 should not be subscribed after removal")
	}

	matches := m.Publish("a")
	if len(matches) != 1 || matches[0].ConnID != "conn2" {
		t.Errorf("after removal, publish to 'a' = %v, want [conn2]", matches)
	}

	matches = m.Publish("c.test")
	if len(matches) != 0 {
		t.Error("pattern subscription should be removed")
	}
}

func TestRemoveConnectionIdempotent(t *testing.T) {
	m := NewSubscriptionManager()
	m.RemoveConnection("nonexistent")
}

func TestSubscriptionCount(t *testing.T) {
	m := NewSubscriptionManager()

	m.Subscribe("conn1", "a")
	m.PSubscribe("conn1", "b.*")
	if m.SubscriptionCount("conn1") != 2 {
		t.Errorf("count = %d, want 2", m.SubscriptionCount("conn1"))
	}

	if m.SubscriptionCount("nonexistent") != 0 {
		t.Error("nonexistent connection should have 0 subscriptions")
	}
}

func TestDuplicateSubscribe(t *testing.T) {
	m := NewSubscriptionManager()
	m.Subscribe("conn1", "ch")
	m.Subscribe("conn1", "ch")
	if m.SubscriptionCount("conn1") != 1 {
		t.Errorf("duplicate subscribe should not increase count, got %d", m.SubscriptionCount("conn1"))
	}
}
