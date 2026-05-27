package jupyter

import (
	"sync"
	"sync/atomic"
)

// iopubRouter fans iopub messages out to per-execution subscribers,
// keyed by parent_header.msg_id. The kernel publishes every status /
// stream / result message on a single PUB socket; we demultiplex.
//
// Backpressure: each subscriber owns a buffered channel. If it can't
// keep up the router drops messages and increments a per-subscription
// counter so callers can surface "logs were lossy" to the user instead
// of silently dropping bytes.
type iopubRouter struct {
	mu   sync.RWMutex
	subs map[string]*subscription

	// broadcastListeners receive every iopub message regardless of
	// parent. Used by long-lived telemetry hooks; the execute path
	// uses Subscribe instead.
	broadcasters []chan *Message
}

type subscription struct {
	ch      chan *Message
	dropped atomic.Int64
}

func newIOPubRouter() *iopubRouter {
	return &iopubRouter{subs: make(map[string]*subscription)}
}

// Subscribe registers a buffered channel keyed by parentMsgID. Returns
// the channel and an unsubscribe func. buf is the channel capacity;
// 256 is a sensible default for streaming print spam.
func (r *iopubRouter) Subscribe(parentMsgID string, buf int) (<-chan *Message, func() int64) {
	if buf <= 0 {
		buf = 256
	}
	s := &subscription{ch: make(chan *Message, buf)}
	r.mu.Lock()
	r.subs[parentMsgID] = s
	r.mu.Unlock()

	unsub := func() int64 {
		r.mu.Lock()
		delete(r.subs, parentMsgID)
		r.mu.Unlock()
		close(s.ch)
		return s.dropped.Load()
	}
	return s.ch, unsub
}

// Dispatch routes msg to its subscriber (if any) and to every
// broadcaster. Non-blocking — overflow is recorded, not awaited.
func (r *iopubRouter) Dispatch(msg *Message) {
	pid := msg.ParentMsgID()

	r.mu.RLock()
	s := r.subs[pid]
	bcs := r.broadcasters
	r.mu.RUnlock()

	if s != nil {
		select {
		case s.ch <- msg:
		default:
			s.dropped.Add(1)
		}
	}
	for _, bc := range bcs {
		select {
		case bc <- msg:
		default:
			// broadcasters are best-effort; no per-listener counter.
		}
	}
}

// CloseAll shuts every subscription channel down. Used during kernel
// shutdown so blocked Execute calls unwind.
func (r *iopubRouter) CloseAll() {
	r.mu.Lock()
	subs := r.subs
	r.subs = make(map[string]*subscription)
	r.mu.Unlock()
	for _, s := range subs {
		close(s.ch)
	}
}
