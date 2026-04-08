package blocking

import "sync"

// WakeResult carries the key and value delivered to an unblocked client.
type WakeResult struct {
	Key   string
	Value string
}

type waiter struct {
	ch   chan WakeResult
	keys []string
}

// Registry tracks goroutines that are blocking on one or more list keys.
type Registry struct {
	mu      sync.Mutex
	waiters map[string][]*waiter
	done    chan struct{}
}

// NewRegistry creates an empty Registry ready for use.
func NewRegistry() *Registry {
	return &Registry{
		waiters: make(map[string][]*waiter),
		done:    make(chan struct{}),
	}
}

// Done returns a channel that is closed when the registry is shut down.
func (r *Registry) Done() <-chan struct{} {
	return r.done
}

// Register creates a waiter on the given keys. It returns the waiter's result
// channel and a cancel function that removes the waiter from all keys.
func (r *Registry) Register(keys []string) (chan WakeResult, func()) {
	ch := make(chan WakeResult, 1)
	w := &waiter{ch: ch, keys: keys}

	r.mu.Lock()
	for _, key := range keys {
		r.waiters[key] = append(r.waiters[key], w)
	}
	r.mu.Unlock()

	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, key := range w.keys {
			waiters := r.waiters[key]
			for i, ww := range waiters {
				if ww == w {
					r.waiters[key] = append(waiters[:i], waiters[i+1:]...)
					break
				}
			}
			if len(r.waiters[key]) == 0 {
				delete(r.waiters, key)
			}
		}
	}

	return ch, cancel
}

// TryWake removes the first waiter (FIFO) for the given key and also removes
// it from every other key it was registered on. Returns false if no waiters
// exist for that key.
func (r *Registry) TryWake(key string) (chan WakeResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	waiters := r.waiters[key]
	if len(waiters) == 0 {
		return nil, false
	}

	// Take the first waiter (FIFO).
	w := waiters[0]

	// Remove this waiter from ALL keys it was registered on.
	for _, k := range w.keys {
		list := r.waiters[k]
		for i, ww := range list {
			if ww == w {
				r.waiters[k] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(r.waiters[k]) == 0 {
			delete(r.waiters, k)
		}
	}

	return w.ch, true
}

// Shutdown closes the done channel, unblocking all waiting goroutines.
func (r *Registry) Shutdown() {
	close(r.done)
}
