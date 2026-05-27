package tunnel

import "errors"

// Public sentinel errors. Callers compare with errors.Is so we can wrap
// these with context-specific detail in the dialer/listener paths.
var (
	// ErrHandshakeFailed is returned when the HTTP PUT that precedes the
	// yamux upgrade got a non-200 response (bad cert, missing auth, the
	// broker rejected the device identity, …). The wrapped error carries
	// the HTTP status code or transport error so callers can decide
	// whether to retry.
	ErrHandshakeFailed = errors.New("tunnel: handshake failed")

	// ErrSessionClosed is returned by RoundTrip / OpenStream when the
	// yamux session is no longer usable. The caller should rebuild the
	// session (typically via Backoff).
	ErrSessionClosed = errors.New("tunnel: session closed")

	// ErrNoHijacker is returned by Accept when the http.ResponseWriter
	// doesn't support hijacking. Indicates a misconfigured server (e.g.
	// HTTP/2 without h2c, or a middleware stripped the hijacker).
	ErrNoHijacker = errors.New("tunnel: response writer does not support hijack")
)
