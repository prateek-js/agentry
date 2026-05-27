package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// HandshakeHeader is the HTTP header the dialer uses to identify which
// side of the tunnel it is. Brokers route on this — a "device" session
// goes into the user directory, a "cluster" session goes into the
// provisioner directory.
const HandshakeHeader = "X-Tunnel-Role"

// HeaderDeviceID is the handshake header carrying the user's device
// identifier on a role=device dial. The broker registers the session
// under this ID in its device directory.
const HeaderDeviceID = "X-Device-ID"

// HeaderClusterID is the handshake header carrying the cluster
// identifier on a role=cluster dial. The broker registers the session
// under this ID in its cluster directory.
const HeaderClusterID = "X-Cluster-ID"

// Role identifies which side of the broker the dialer is. Defined as a
// type rather than a bare string so the compiler catches typos.
type Role string

const (
	// RoleDevice is used by the xdp daemon on the user's machine.
	RoleDevice Role = "device"
	// RoleCluster is used by a per-cluster provisioner phoning home to
	// the broker. Same wire shape, different routing table on the broker.
	RoleCluster Role = "cluster"
)

// DialConfig is everything Dial needs to bring up a tunnel session.
type DialConfig struct {
	// BrokerURL is the https://… address the broker listens on. The
	// path is appended automatically (default "/tunnel").
	BrokerURL string

	// Path is the request path used in the HTTP handshake. Empty falls
	// back to "/tunnel".
	Path string

	// TLSConfig is the mTLS config — must carry the device or cluster
	// cert in Certificates, and the broker's CA in RootCAs. nil means
	// plaintext, which is only sane in tests.
	TLSConfig *tls.Config

	// Role is what the broker will route on. Required.
	Role Role

	// Headers are extra HTTP headers added to the handshake request.
	// Useful for carrying a one-shot bootstrap token, a device ID, the
	// current cluster context — anything the broker authenticates on
	// beyond the cert itself.
	Headers http.Header

	// HandshakeTimeout caps the HTTP handshake before yamux takes over.
	// Default 30s. Once yamux is up, this no longer applies.
	HandshakeTimeout time.Duration

	// YamuxConfig overrides the default yamux settings. nil = sensible
	// defaults (keepalive on, 15s interval, 60s write timeout).
	YamuxConfig *yamux.Config
}

// Dial brings up one tunnel session against the broker. Returns the
// yamux session; the caller is responsible for closing it (or, more
// usually, watching session.CloseChan() and rebuilding via Backoff).
//
// The handshake is one HTTP PUT to BrokerURL+Path. We expect 200 OK
// and then we hijack the connection — anything else, including 4xx and
// 5xx, returns ErrHandshakeFailed.
func Dial(ctx context.Context, cfg DialConfig) (*yamux.Session, error) {
	if cfg.Role == "" {
		return nil, fmt.Errorf("tunnel.Dial: role is required")
	}
	if cfg.BrokerURL == "" {
		return nil, fmt.Errorf("tunnel.Dial: broker URL is required")
	}
	u, err := url.Parse(cfg.BrokerURL)
	if err != nil {
		return nil, fmt.Errorf("tunnel.Dial: bad broker URL %q: %w", cfg.BrokerURL, err)
	}
	path := cfg.Path
	if path == "" {
		path = "/tunnel"
	}
	timeout := cfg.HandshakeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	host := u.Host
	if u.Port() == "" {
		// url.Parse normalizes default ports out; put them back so
		// net.Dial gets host:port consistently.
		if u.Scheme == "https" {
			host = host + ":443"
		} else {
			host = host + ":80"
		}
	}

	// Dial the raw TCP/TLS conn ourselves so we can hand the post-
	// hijack socket directly to yamux. We can't use net/http's client
	// because once the body is read we've lost access to the conn.
	dialer := &net.Dialer{Timeout: timeout}
	var raw net.Conn
	switch u.Scheme {
	case "https":
		raw, err = tls.DialWithDialer(dialer, "tcp", host, cfg.TLSConfig)
	case "http":
		raw, err = dialer.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("tunnel.Dial: unsupported scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, fmt.Errorf("tunnel.Dial: dial %s: %w", host, err)
	}
	// From here on, raw is owned by yamux on success or closed on failure.

	if err := writeHandshakeRequest(raw, u.Host, path, cfg.Role, cfg.Headers); err != nil {
		_ = raw.Close()
		return nil, err
	}
	// Read the response by hand. http.ReadResponse would configure a
	// body reader; closing that body invokes io.Copy(discard, body)
	// which would then consume the yamux frames the broker has already
	// started sending on the same conn. Parsing the status line + the
	// headers ourselves keeps yamux's first byte exactly where it
	// landed in the bufio buffer (replayed via drainedConn below).
	br := bufio.NewReader(raw)
	if err := readHandshakeResponse(br); err != nil {
		_ = raw.Close()
		return nil, err
	}

	// Hand the conn to yamux. If bufio buffered any bytes past the
	// response headers (it shouldn't, the broker doesn't send a body
	// before yamux frames), prepend them.
	conn := drainedConn(raw, br)
	yCfg := cfg.YamuxConfig
	if yCfg == nil {
		yCfg = defaultYamuxConfig()
	}
	session, err := yamux.Client(conn, yCfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tunnel.Dial: yamux setup: %w", err)
	}
	return session, nil
}

// AcceptConfig is what the broker side hands to Accept when an HTTP
// handler matches /tunnel.
type AcceptConfig struct {
	// YamuxConfig overrides the default yamux settings. nil = defaults.
	YamuxConfig *yamux.Config
}

// Accept performs the server side of the handshake. It hijacks the
// underlying conn, runs yamux.Server on it, and returns the session.
//
// The caller (broker) is expected to have already authenticated the
// request — checked the mTLS cert, validated the role header, looked
// up the org / cluster. Accept only does the transport-level upgrade;
// it doesn't enforce policy.
//
// On any failure before yamux is up, Accept writes an HTTP error to
// the ResponseWriter; on success, it has already taken over the conn
// and the caller MUST NOT write to ResponseWriter further.
func Accept(w http.ResponseWriter, r *http.Request, cfg AcceptConfig) (*yamux.Session, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return nil, ErrNoHijacker
	}

	// Reply 200 before hijack. Explicitly set Content-Length: 0 (not
	// "Connection: close") so the client's HTTP response parser
	// classifies the body as empty rather than "read until EOF". With
	// the latter, the client would try to drain the body and eat the
	// yamux frames we're about to write. After hijack, we can't write
	// headers — so this is our only chance to tell the client the
	// shape of the (empty) body.
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
	// Force the headers to flush onto the wire before we take over.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack: %w", err)
	}
	// Disable per-conn deadlines; yamux manages liveness from here.
	_ = conn.SetDeadline(time.Time{})

	// If the bufio writer has buffered bytes (it shouldn't, we already
	// Flushed), get them out before yamux starts reading.
	if brw != nil && brw.Writer != nil {
		if err := brw.Writer.Flush(); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("hijack flush: %w", err)
		}
	}
	final := conn
	if brw != nil && brw.Reader != nil && brw.Reader.Buffered() > 0 {
		final = drainedConn(conn, brw.Reader)
	}

	yCfg := cfg.YamuxConfig
	if yCfg == nil {
		yCfg = defaultYamuxConfig()
	}
	session, err := yamux.Server(final, yCfg)
	if err != nil {
		_ = final.Close()
		return nil, fmt.Errorf("yamux server: %w", err)
	}
	return session, nil
}

// readHandshakeResponse reads the status line + headers from a fresh
// connection and validates the status is 200. We don't use
// http.ReadResponse because it constructs a body reader whose
// subsequent Close() drains the conn — and after this handshake the
// conn carries yamux frames, not HTTP body bytes.
func readHandshakeResponse(br *bufio.Reader) error {
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("%w: reading status line: %v", ErrHandshakeFailed, err)
	}
	// Drain headers until empty line. We don't parse them — the broker
	// has already authenticated the request via mTLS / role header in
	// the request direction; the response is just "ok, take over".
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("%w: reading headers: %v", ErrHandshakeFailed, err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	// Status line is "HTTP/1.1 200 OK\r\n". Anything else is rejection.
	if !strings.Contains(statusLine, " 200 ") {
		return fmt.Errorf("%w: %s", ErrHandshakeFailed, strings.TrimSpace(statusLine))
	}
	return nil
}

// writeHandshakeRequest issues a minimal HTTP/1.1 PUT request directly
// on the wire. We don't use http.Request.Write because we need control
// over exactly what's on the conn so the hijack on the other side
// doesn't read extra bytes.
func writeHandshakeRequest(c net.Conn, host, path string, role Role, headers http.Header) error {
	req := &http.Request{
		Method: "PUT",
		URL:    &url.URL{Path: path},
		Host:   host,
		Header: http.Header{},
	}
	req.Header.Set("Connection", "close")
	req.Header.Set("User-Agent", "ad-sandbox-tunnel/1")
	req.Header.Set(HandshakeHeader, string(role))
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if err := req.Write(c); err != nil {
		return fmt.Errorf("%w: writing handshake request: %v", ErrHandshakeFailed, err)
	}
	return nil
}

// drainedConn returns a net.Conn that first replays whatever br has
// already buffered before falling through to c. We need this because
// bufio.Reader sits in front of the hijacked socket and may have
// pulled bytes that yamux now needs to read.
func drainedConn(c net.Conn, br *bufio.Reader) net.Conn {
	if br == nil || br.Buffered() == 0 {
		return c
	}
	return &drainConn{Conn: c, br: br}
}

type drainConn struct {
	net.Conn
	br *bufio.Reader
}

func (d *drainConn) Read(p []byte) (int, error) {
	if d.br != nil && d.br.Buffered() > 0 {
		n, err := d.br.Read(p)
		if d.br.Buffered() == 0 {
			d.br = nil
		}
		return n, err
	}
	return d.Conn.Read(p)
}

func defaultYamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = 60 * time.Second
	// yamux logs to stderr by default. Silence it; the broker / xdp
	// logs at their own layer.
	c.Logger = nil
	c.LogOutput = nilWriter{}
	return c
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }

// Compile-time witness that we satisfy net.Conn for drainConn.
var (
	_ net.Conn = (*drainConn)(nil)
	_ error    = errors.New("")
)
