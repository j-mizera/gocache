package events

import (
	apiEvents "gocache/api/events"
)

// ring is a bounded FIFO buffer of recent events used to replay history to
// subscribers that attach after events were already emitted — this is what
// lets an IPC plugin connecting at t=500ms still observe the t=0 boot
// events the embedded observability tier has already seen.
//
// Overflow drops the oldest entry and increments dropped so the next
// subscriber can be told how much they missed via a ReplayGap event.
//
// All methods are called under Bus.mu — the ring itself holds no lock.
type ring struct {
	buf     []apiEvents.Event // fixed-cap slice, never resized after construction
	head    int               // index of the oldest entry in buf
	size    int               // number of live entries; always <= cap(buf)
	dropped uint64            // cumulative count of entries evicted by overflow
}

// newRing returns a ring with capacity cap. cap<=0 disables replay.
func newRing(cap int) *ring {
	if cap <= 0 {
		return &ring{}
	}
	return &ring{buf: make([]apiEvents.Event, cap)}
}

// enabled reports whether this ring retains anything.
func (r *ring) enabled() bool { return len(r.buf) > 0 }

// push records evt. When the ring is full the oldest entry is dropped.
func (r *ring) push(evt apiEvents.Event) {
	if !r.enabled() {
		return
	}
	cap := len(r.buf)
	if r.size == cap {
		// Overwrite the oldest slot; advance head.
		r.buf[r.head] = evt
		r.head = (r.head + 1) % cap
		r.dropped++
		return
	}
	tail := (r.head + r.size) % cap
	r.buf[tail] = evt
	r.size++
}

// snapshot returns a FIFO-ordered copy of retained events plus the dropped
// count at snapshot time. The returned slice is owned by the caller and
// safe to iterate without holding any lock.
func (r *ring) snapshot() ([]apiEvents.Event, uint64) {
	if !r.enabled() || r.size == 0 {
		return nil, r.dropped
	}
	out := make([]apiEvents.Event, r.size)
	cap := len(r.buf)
	for i := range r.size {
		out[i] = r.buf[(r.head+i)%cap]
	}
	return out, r.dropped
}
