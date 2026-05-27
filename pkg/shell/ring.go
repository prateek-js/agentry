package shell

import "sync"

// Ring is a fixed-capacity byte buffer that retains only the most recent
// `cap` bytes written to it. It is the storage primitive for background
// command logs: stdout/stderr stream into Write, HTTP polls call Read
// with a cursor and get back exactly the bytes they haven't seen yet.
//
// The buffer tracks two monotonic counters:
//
//	written  total bytes ever Write'd (the "end cursor")
//	dropped  total bytes evicted because the ring was full
//
// A reader's cursor is an absolute byte offset since command start.
// When cursor < (written - len(currentBytes)), some bytes have been
// evicted before the reader could see them; Read returns how many.
//
// Ring is safe for concurrent use.
type Ring struct {
	mu      sync.Mutex
	buf     []byte
	head    int   // index of the oldest byte in buf
	size    int   // current number of valid bytes
	cap     int   // max capacity (>0; immutable)
	written int64 // total bytes ever written (monotonic)
	dropped int64 // total bytes evicted (monotonic)
}

// NewRing creates an empty Ring with the given capacity. cap must be > 0.
// The underlying buffer is allocated lazily on first Write so idle rings
// hold no memory.
func NewRing(cap int) *Ring {
	if cap <= 0 {
		panic("shell.NewRing: cap must be > 0")
	}
	return &Ring{cap: cap}
}

// Write appends p to the ring, evicting oldest bytes when necessary so
// total stored never exceeds cap. Always returns len(p), nil — matches
// the io.Writer contract for an in-memory sink.
func (r *Ring) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(p)
	r.written += int64(n)

	// Allocate the backing array on first write.
	if r.buf == nil {
		r.buf = make([]byte, r.cap)
	}

	// If the incoming chunk is larger than capacity, only the last cap
	// bytes will fit. Everything we currently hold plus the leading
	// (n-cap) bytes of p are dropped.
	if n >= r.cap {
		r.dropped += int64(r.size) + int64(n-r.cap)
		copy(r.buf, p[n-r.cap:])
		r.head = 0
		r.size = r.cap
		return n, nil
	}

	// Otherwise drop just enough bytes from the front to make room.
	if r.size+n > r.cap {
		overflow := r.size + n - r.cap
		r.dropped += int64(overflow)
		r.head = (r.head + overflow) % r.cap
		r.size -= overflow
	}

	tail := (r.head + r.size) % r.cap
	first := r.cap - tail
	if first >= n {
		copy(r.buf[tail:], p)
	} else {
		copy(r.buf[tail:], p[:first])
		copy(r.buf, p[first:])
	}
	r.size += n
	return n, nil
}

// Read returns the bytes written since `cursor`, the new cursor (the
// total bytes ever written so far), and how many bytes were dropped
// before this read could see them. A return of (nil, written, 0)
// means the reader is caught up.
//
// Callers should treat dropped > 0 as a gap — the data is irrecoverably
// gone. Detecting it lets callers surface it to the user instead of
// silently presenting a discontinuous transcript.
func (r *Ring) Read(cursor int64) (data []byte, newCursor int64, dropped int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	oldest := r.written - int64(r.size)

	start := cursor
	if start < oldest {
		dropped = oldest - start
		start = oldest
	}
	if start >= r.written {
		// Reader is caught up. May still report dropped from above.
		return nil, r.written, dropped
	}

	n := int(r.written - start)
	out := make([]byte, n)
	relStart := int(start - oldest)
	ringIdx := (r.head + relStart) % r.cap

	first := r.cap - ringIdx
	if first >= n {
		copy(out, r.buf[ringIdx:ringIdx+n])
	} else {
		copy(out[:first], r.buf[ringIdx:])
		copy(out[first:], r.buf[:n-first])
	}
	return out, r.written, dropped
}

// Snapshot returns the entire current contents (up to cap bytes) plus
// the new cursor and dropped count. Convenience over Read(0).
func (r *Ring) Snapshot() (data []byte, written int64, dropped int64) {
	return r.Read(0)
}

// Written returns the total bytes ever written. Cheap; for status/UI.
func (r *Ring) Written() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written
}

// Dropped returns the total bytes evicted. Cheap; for status/UI.
func (r *Ring) Dropped() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}
