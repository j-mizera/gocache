package main

import "sync"

type SubscriptionManager struct {
	mu           sync.RWMutex
	channels     map[string]map[string]struct{} // channel → connID set
	patterns     map[string]map[string]struct{} // pattern → connID set
	connChannels map[string]map[string]struct{} // connID → channel set
	connPatterns map[string]map[string]struct{} // connID → pattern set
}

func NewSubscriptionManager() *SubscriptionManager {
	return &SubscriptionManager{
		channels:     make(map[string]map[string]struct{}),
		patterns:     make(map[string]map[string]struct{}),
		connChannels: make(map[string]map[string]struct{}),
		connPatterns: make(map[string]map[string]struct{}),
	}
}

func (m *SubscriptionManager) Subscribe(connID, channel string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addToSet(m.channels, channel, connID)
	addToSet(m.connChannels, connID, channel)
}

func (m *SubscriptionManager) Unsubscribe(connID, channel string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	removeFromSet(m.channels, channel, connID)
	removeFromSet(m.connChannels, connID, channel)
}

func (m *SubscriptionManager) PSubscribe(connID, pattern string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addToSet(m.patterns, pattern, connID)
	addToSet(m.connPatterns, connID, pattern)
}

func (m *SubscriptionManager) PUnsubscribe(connID, pattern string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	removeFromSet(m.patterns, pattern, connID)
	removeFromSet(m.connPatterns, connID, pattern)
}

// Channels returns a copy of the channels the connection is subscribed to.
func (m *SubscriptionManager) Channels(connID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return setKeys(m.connChannels[connID])
}

// Patterns returns a copy of the patterns the connection is subscribed to.
func (m *SubscriptionManager) Patterns(connID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return setKeys(m.connPatterns[connID])
}

type PublishMatch struct {
	ConnID  string
	Pattern string // non-empty for pattern matches
}

// Publish returns all connections that should receive a message on channel:
// exact channel subscribers + pattern subscribers whose pattern matches.
func (m *SubscriptionManager) Publish(channel string) []PublishMatch {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var matches []PublishMatch

	for connID := range m.channels[channel] {
		matches = append(matches, PublishMatch{ConnID: connID})
	}

	for pattern, conns := range m.patterns {
		if matchPattern(pattern, channel) {
			for connID := range conns {
				matches = append(matches, PublishMatch{ConnID: connID, Pattern: pattern})
			}
		}
	}

	return matches
}

func (m *SubscriptionManager) IsSubscribed(connID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.connChannels[connID]) > 0 || len(m.connPatterns[connID]) > 0
}

// SubscriptionCount returns the total number of subscriptions for a connection.
func (m *SubscriptionManager) SubscriptionCount(connID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.connChannels[connID]) + len(m.connPatterns[connID])
}

// RemoveConnection removes all subscriptions for a connection.
func (m *SubscriptionManager) RemoveConnection(connID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for ch := range m.connChannels[connID] {
		removeFromSet(m.channels, ch, connID)
	}
	delete(m.connChannels, connID)

	for pat := range m.connPatterns[connID] {
		removeFromSet(m.patterns, pat, connID)
	}
	delete(m.connPatterns, connID)
}

func addToSet(m map[string]map[string]struct{}, key, val string) {
	s, ok := m[key]
	if !ok {
		s = make(map[string]struct{})
		m[key] = s
	}
	s[val] = struct{}{}
}

func removeFromSet(m map[string]map[string]struct{}, key, val string) {
	s, ok := m[key]
	if !ok {
		return
	}
	delete(s, val)
	if len(s) == 0 {
		delete(m, key)
	}
}

func setKeys(s map[string]struct{}) []string {
	if len(s) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	return keys
}
