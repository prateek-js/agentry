package tunnel

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/hashicorp/yamux"
)

// NewRoundTripper returns an http.RoundTripper that opens a fresh
// yamux stream per HTTP request, writes the request to it, and reads
// the response back from the same stream. The Host in the URL is
// ignored — every request goes to whoever is on the other end of the
// session — but the path and headers go through verbatim.
//
// Wiring this into the existing MCP / provisioner clients is a
// drop-in: build an *http.Client{Transport: rt} and the rest of the
// code stack doesn't change.
//
// The session is held by pointer (via *atomic.Pointer) so a caller
// running a reconnect loop can swap in a fresh session after a tunnel
// drop without rebuilding the http.Client.
type RoundTripper struct {
	session atomic.Pointer[yamux.Session]
}

// NewRoundTripper builds a RoundTripper around the given session.
// SetSession can be called later to rotate after a reconnect.
func NewRoundTripper(s *yamux.Session) *RoundTripper {
	rt := &RoundTripper{}
	rt.session.Store(s)
	return rt
}

// SetSession swaps the underlying yamux session atomically. Existing
// in-flight requests continue using the old session (it's referenced
// by their open streams); new requests use the new one.
func (rt *RoundTripper) SetSession(s *yamux.Session) {
	rt.session.Store(s)
}

// Session returns the current session. nil means we've never been
// given one, which only happens if a caller used the zero value.
func (rt *RoundTripper) Session() *yamux.Session {
	return rt.session.Load()
}

// RoundTrip implements http.RoundTripper. Each call opens one yamux
// stream, writes the request, and reads the response. The response
// body wraps the stream so closing the body closes the stream.
func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	sess := rt.session.Load()
	if sess == nil || sess.IsClosed() {
		return nil, fmt.Errorf("%w: no live session", ErrSessionClosed)
	}

	stream, err := sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("%w: open stream: %v", ErrSessionClosed, err)
	}

	// http.Request.Write needs Host; the body we'll let it handle.
	if req.Host == "" && req.URL != nil {
		req.Host = req.URL.Host
	}
	if err := req.Write(stream); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write request: %w", err)
	}

	br := bufio.NewReader(stream)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}

	// HTTP Upgrade (WebSocket, h2c): http.ReadResponse sets Body to
	// NoBody for 1xx responses, which is wrong for a 101 — the post-
	// header bytes are the actual upgraded payload. Replace with a
	// body that reads the bufio remainder first, then the raw stream,
	// and writes go straight to the stream. That satisfies the
	// io.ReadWriteCloser contract that httputil.ReverseProxy's
	// upgrade pump asserts on.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body = &upgradeBody{br: br, stream: stream}
	} else {
		resp.Body = &streamBody{ReadCloser: resp.Body, stream: stream}
	}
	return resp, nil
}

// upgradeBody is what wraps a 101 response. Read drains the bufio
// reader the response headers were parsed through (it may have
// buffered post-header bytes) before falling through to the raw
// stream. Write goes straight to the stream. Close tears down the
// stream — and skips the drain that streamBody does, because on an
// upgraded conn there's no clean EOF to wait for and the drain would
// hang forever.
type upgradeBody struct {
	br     *bufio.Reader
	stream *yamux.Stream
}

func (u *upgradeBody) Read(p []byte) (int, error) {
	if u.br != nil && u.br.Buffered() > 0 {
		n, err := u.br.Read(p)
		if u.br.Buffered() == 0 {
			u.br = nil
		}
		return n, err
	}
	return u.stream.Read(p)
}

func (u *upgradeBody) Write(p []byte) (int, error) {
	return u.stream.Write(p)
}

func (u *upgradeBody) Close() error {
	return u.stream.Close()
}

// streamBody is the response body wrapper. It exposes Read+Write+Close
// so it satisfies io.ReadWriteCloser, which is what
// httputil.ReverseProxy type-asserts on for HTTP upgrade (WebSocket)
// responses: it pipes the Body and the hijacked client conn together
// as raw bytes once it sees a 101 Switching Protocols.
//
// Read goes through the http.Response body reader (which already
// knows how to drain whatever bufio.Reader buffered past the headers
// during http.ReadResponse). Write goes straight to the yamux stream
// — there's no http response body writer to defer to; for non-upgrade
// requests no one ever calls Write so this is dead code on the happy
// path.
type streamBody struct {
	io.ReadCloser
	stream *yamux.Stream
}

// Write satisfies the upgrade contract. For ordinary HTTP responses
// no caller writes to a body; only ReverseProxy on a 101 path does.
func (b *streamBody) Write(p []byte) (int, error) {
	return b.stream.Write(p)
}

func (b *streamBody) Close() error {
	// Drain so the peer sees EOF cleanly before we tear down the
	// stream — a peer mid-flush would otherwise see a RST. 101 uses
	// the separate upgradeBody type and never lands here, so the
	// drain is safe (it terminates on EOF for normal HTTP bodies).
	_, _ = io.Copy(io.Discard, b.ReadCloser)
	bodyErr := b.ReadCloser.Close()
	streamErr := b.stream.Close()
	if bodyErr != nil {
		return bodyErr
	}
	return streamErr
}

// Compile-time witness.
var _ http.RoundTripper = (*RoundTripper)(nil)
