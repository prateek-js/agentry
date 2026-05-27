// Package tunnel is the wire-protocol foundation shared by the xdp client,
// the xdp-broker, and the per-cluster provisioner.
//
// The shape is one yamux session per long-lived peer connection (xdp daemon
// ↔ broker, provisioner ↔ broker), with each logical request — an MCP tool
// call, a provisioner API hit — riding on its own yamux stream. yamux gives
// us multiplexing, flow control, and keep-alive over a single mTLS TCP
// connection; the application layer sees per-call streams as cheap as Go
// channels.
//
// The handshake is HTTP-first: the dialer sends a normal HTTP PUT (so the
// connection traverses corporate proxies and works with Let's Encrypt), the
// server replies with the standard "Connection: close, 200 OK" and then
// Hijacks the underlying TCP/TLS conn. Both sides put yamux on top. After
// that the HTTP layer is gone and the conn is a multiplexer until close.
//
// The package is deliberately small. It owns the bytes-on-the-wire concerns
// only: connection wrapping, bidirectional copy, exponential backoff, the
// hijack handshake, and an http.RoundTripper that opens a fresh yamux
// stream per request. Identity (certs, CA, RBAC) and routing (which
// provisioner serves which cluster) live one layer up.
package tunnel
