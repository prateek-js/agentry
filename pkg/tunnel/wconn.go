package tunnel

import (
	"net"
	"sync"
)

// WConn wraps a net.Conn with a one-shot close callback. We use it to
// observe tunnel teardown from above the yamux layer: the dialer/listener
// passes a callback that updates session state (drops from the session
// directory, triggers reconnect logic) without poking at yamux internals.
//
// The callback fires at most once even if Close is called repeatedly —
// idempotency matters because yamux on shutdown will Close the underlying
// conn from multiple goroutines as it tears down streams.
type WConn struct {
	net.Conn

	onClose func()
	once    sync.Once
}

// NewWConn wraps c. onClose may be nil, in which case Close just closes
// the underlying conn.
func NewWConn(c net.Conn, onClose func()) *WConn {
	return &WConn{Conn: c, onClose: onClose}
}

// Close closes the underlying conn and fires the onClose hook exactly
// once. The error from the underlying Close is preserved.
func (w *WConn) Close() error {
	err := w.Conn.Close()
	w.once.Do(func() {
		if w.onClose != nil {
			w.onClose()
		}
	})
	return err
}

// SetDeadline / SetReadDeadline / SetWriteDeadline are inherited from
// the embedded net.Conn. We don't need to intercept them — yamux only
// touches deadlines on its control plane and we want that pass-through.

// Static interface witness. The dialer hands these to yamux.Client /
// yamux.Server, both of which take a net.Conn.
var _ net.Conn = (*WConn)(nil)
