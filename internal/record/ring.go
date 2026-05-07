package record

import "sync"

// Ring is a fixed-capacity circular buffer of Flow records used for
// sub-second live views (the "hot" tier in VISION.md §3.3). Push
// overwrites the oldest entry when the buffer is full; Snapshot returns
// a stable copy of the contents in oldest-first order.
//
// Ring is safe for concurrent Push and Snapshot. Lock contention is
// negligible relative to the cost of a syscall — the parser pool can
// drive multiple goroutines into Push without serializing.
type Ring struct {
	mu   sync.RWMutex // guards buf, head, full
	buf  []Flow
	head int
	full bool
	cap  int
}

// NewRing returns a Ring of the given capacity. Capacity must be > 0.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		panic("record: ring capacity must be > 0")
	}
	return &Ring{
		buf: make([]Flow, capacity),
		cap: capacity,
	}
}

// Cap returns the configured capacity.
func (r *Ring) Cap() int { return r.cap }

// Push appends a flow, evicting the oldest entry if the ring is at
// capacity. Push is non-blocking and never returns an error — the ring
// is best-effort by design.
func (r *Ring) Push(f Flow) {
	r.mu.Lock()
	r.buf[r.head] = f
	r.head++
	if r.head == r.cap {
		r.head = 0
		r.full = true
	}
	r.mu.Unlock()
}

// Len returns the number of records currently held (≤ Cap).
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.full {
		return r.cap
	}
	return r.head
}

// Snapshot returns a copy of all currently-held records, oldest first.
// The returned slice is independent of the ring and safe to retain
// across subsequent Push calls.
func (r *Ring) Snapshot() []Flow {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.full {
		out := make([]Flow, r.head)
		copy(out, r.buf[:r.head])
		return out
	}
	out := make([]Flow, r.cap)
	copy(out, r.buf[r.head:])
	copy(out[r.cap-r.head:], r.buf[:r.head])
	return out
}
