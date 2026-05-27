package tunnel

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// CONNECT is the data-plane verb. Every byte-forwarded TCP connection
// (xdp forward, psql, redis-cli, ssh, debugger attach, anything that
// dials a localhost:N inside a sandbox) rides an HTTP/1.1 CONNECT
// request at every hop of the tunnel:
//
//	xdp ──CONNECT──► broker ──CONNECT──► provisioner ──CONNECT──► runtime
//
// Each hop reads the CONNECT, decides where to forward (per its
// routing role), opens the next hop, writes its own CONNECT, reads a
// 200 OK reply, and then byte-pumps the rest with CopyStreams. No
// HTTP framing on the bytes that follow.
//
// HTTP control-plane RPCs (MCP tool calls, provisioner API) stay
// HTTP and use a completely separate code path at each layer. The
// HTTP method on the inbound side is the demux.

// HeaderForwardSandbox is added by the broker when relaying a
// CONNECT to the provisioner — it carries the target sandbox id
// since the request-line target is "sandbox:port" but the
// provisioner needs sandbox alone for the lookup.
const HeaderForwardSandbox = "X-Forward-Sandbox"

// WriteConnect issues a CONNECT request on w. target is the
// "host:port" form RFC 7231 expects in the request line. Extra
// headers go through verbatim — broker / provisioner use this to
// thread X-Cluster, X-Forwarded-Device, request IDs.
func WriteConnect(w io.Writer, target string, headers http.Header) error {
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\n", target)
	fmt.Fprintf(&b, "Host: %s\r\n", target)
	for k, vs := range headers {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write CONNECT: %w", err)
	}
	return nil
}

// ReadConnectResponse parses the status line + headers of a CONNECT
// response. Returns nil on 200, an error otherwise. The bufio.Reader
// keeps any bytes that arrived past the empty header line — callers
// who fall through to byte-pumping must replay those (use
// drainedReadWriteCloser).
func ReadConnectResponse(r *bufio.Reader) error {
	statusLine, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read status line: %w", err)
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if !strings.Contains(statusLine, " 200 ") {
		return fmt.Errorf("CONNECT rejected: %s", strings.TrimSpace(statusLine))
	}
	return nil
}

// AcceptConnect is the server-side helper used by broker, provisioner,
// and runtime to turn an incoming HTTP CONNECT into a hijacked raw
// conn ready for byte-pumping. Caller passes a `dial` function that
// opens the next hop; on success AcceptConnect writes 200, hijacks
// the inbound conn, and hands back both ends for CopyStreams.
//
// dial returning an error → AcceptConnect writes 502 and returns the
// error without hijacking. The HTTP handler can return cleanly.
func AcceptConnect(w http.ResponseWriter, r *http.Request, dial func() (io.ReadWriteCloser, error)) (inbound net.Conn, upstream io.ReadWriteCloser, err error) {
	if r.Method != http.MethodConnect {
		http.Error(w, "method must be CONNECT", http.StatusBadRequest)
		return nil, nil, fmt.Errorf("not a CONNECT request")
	}
	upstream, err = dial()
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return nil, nil, err
	}

	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		return nil, nil, fmt.Errorf("response writer does not support hijack")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return nil, nil, fmt.Errorf("hijack: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})

	// Replay any bytes the bufio reader buffered past the request
	// line. On a CONNECT the "body" is the byte stream — anything past
	// the empty header line was the client's first frame. Without
	// this replay, those bytes vanish into bufio's internal buffer.
	if brw != nil && brw.Reader != nil && brw.Reader.Buffered() > 0 {
		conn = &drainConn{Conn: conn, br: brw.Reader}
	}
	return conn, upstream, nil
}

// DrainedReadWriteCloser wraps an io.ReadWriteCloser with a bufio
// reader that may have buffered bytes past the response headers. Read
// pulls from the buffer first, then the underlying conn. Used by
// callers of WriteConnect/ReadConnectResponse to feed CopyStreams a
// reader that includes those buffered bytes.
type DrainedReadWriteCloser struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader
}

// NewDrainedReadWriteCloser returns an rwc that first drains br
// (which was used to read CONNECT-response headers from rwc) before
// reading further bytes directly. Writes go straight to rwc.
func NewDrainedReadWriteCloser(rwc io.ReadWriteCloser, br *bufio.Reader) *DrainedReadWriteCloser {
	return &DrainedReadWriteCloser{rwc: rwc, br: br}
}

func (d *DrainedReadWriteCloser) Read(p []byte) (int, error) {
	if d.br != nil && d.br.Buffered() > 0 {
		n, err := d.br.Read(p)
		if d.br.Buffered() == 0 {
			d.br = nil
		}
		return n, err
	}
	return d.rwc.Read(p)
}

func (d *DrainedReadWriteCloser) Write(p []byte) (int, error) { return d.rwc.Write(p) }
func (d *DrainedReadWriteCloser) Close() error                { return d.rwc.Close() }
