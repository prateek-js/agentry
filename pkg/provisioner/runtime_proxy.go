package provisioner

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/tunnel"
)

// runtimeProxyPrefix is the API path under which the provisioner
// reverse-proxies to each sandbox's runtime. The path after the
// sandbox ID and "/runtime" is forwarded verbatim to the runtime.
//
// Example:
//
//	device →  POST /api/sandboxes/sb1/runtime/v1/shell/exec
//	runtime ←      POST                       /v1/shell/exec
//
// This is what makes runtime calls reachable from outside the
// cluster: the runtime port itself is private to the cluster host,
// but the provisioner — which IS reachable, either directly or
// through the broker tunnel — proxies to it.
const runtimeProxyPrefix = "/api/sandboxes/"

// handleRuntimeProxy reverse-proxies a request to the named sandbox's
// runtime. Path shape: /api/sandboxes/{id}/runtime/{rest...}.
//
// The proxy is streaming by default (httputil.ReverseProxy), so SSE
// from /v1/code/exec and WebSocket upgrade on /v1/shell/pty work
// transparently.
func (p *Provisioner) handleRuntimeProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, runtimeProxyPrefix)
	parts := strings.SplitN(rest, "/runtime", 2)
	if len(parts) != 2 {
		http.Error(w, "bad runtime path", http.StatusBadRequest)
		return
	}
	id := parts[0]
	upstreamPath := parts[1]
	if upstreamPath == "" {
		upstreamPath = "/"
	}

	port, err := p.backend.GetNodePort(r.Context(), p.config.Namespace, "sandbox-"+id+"-svc")
	if err != nil || port == 0 {
		http.Error(w, fmt.Sprintf("sandbox %q: not found", id), http.StatusNotFound)
		return
	}

	target, err := url.Parse(fmt.Sprintf("http://%s:%d", p.config.NodeHost, port))
	if err != nil {
		http.Error(w, "bad target URL", http.StatusInternalServerError)
		return
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = upstreamPath
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("runtime proxy upstream error (sandbox=%s path=%s): %v",
				id, upstreamPath, err)
			http.Error(w, "runtime unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// handleSandboxConnect is the provisioner side of the CONNECT
// chain. Request target is "sandbox-id:port" (or "sandbox-id" plus
// header — see below). We:
//
//  1. parse the target
//  2. look up that sandbox's host-mapped runtime port via the backend
//  3. dial the runtime, write a CONNECT to it with the target port,
//     read its 200 OK
//  4. let AcceptConnect hijack the inbound and hand both ends back
//  5. CopyStreams the two together
//
// Auth model: device-can-hit-cluster ⇒ device-can-forward-to-any-
// sandbox-in-it. The cluster check happened already at the broker
// (this provisioner is only reachable via "cluster=ours"). Sandbox
// existence is enforced implicitly by GetNodePort returning 0/err.
func (p *Provisioner) handleSandboxConnect(w http.ResponseWriter, r *http.Request) {
	target := r.RequestURI
	if target == "" {
		target = r.URL.Host
	}
	sandboxID, portStr, ok := strings.Cut(target, ":")
	if !ok || sandboxID == "" || portStr == "" {
		http.Error(w, "CONNECT target must be sandbox:port (got "+target+")", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}

	// Look up the sandbox's runtime host:port. This is the same
	// lookup handleRuntimeProxy uses.
	runtimePort, err := p.backend.GetNodePort(r.Context(), p.config.Namespace, "sandbox-"+sandboxID+"-svc")
	if err != nil || runtimePort == 0 {
		http.Error(w, fmt.Sprintf("sandbox %q not found", sandboxID), http.StatusNotFound)
		return
	}
	runtimeAddr := fmt.Sprintf("%s:%d", p.config.NodeHost, runtimePort)

	dial := func() (io.ReadWriteCloser, error) {
		c, err := net.DialTimeout("tcp", runtimeAddr, 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("dial runtime %s: %w", runtimeAddr, err)
		}
		// CONNECT to the runtime addresses the user's app port —
		// the runtime's listen port is fixed (8080), the CONNECT
		// target tells it which port inside the container to dial.
		runtimeTarget := fmt.Sprintf("127.0.0.1:%d", port)
		if err := tunnel.WriteConnect(c, runtimeTarget, nil); err != nil {
			_ = c.Close()
			return nil, err
		}
		br := bufio.NewReader(c)
		if err := tunnel.ReadConnectResponse(br); err != nil {
			_ = c.Close()
			return nil, err
		}
		return tunnel.NewDrainedReadWriteCloser(c, br), nil
	}

	inbound, upstream, err := tunnel.AcceptConnect(w, r, dial)
	if err != nil {
		log.Printf("provisioner: CONNECT %s: %v", target, err)
		return
	}
	defer inbound.Close()
	defer upstream.Close()
	_ = tunnel.CopyStreams(r.Context(), inbound, upstream, tunnel.CopyOptions{})
}

// sandboxURL returns the URL the provisioner publishes for a freshly-
// created sandbox.
//
// In broker-tunneled deployment (BROKER_URL set) the URL points at
// the per-sandbox runtime proxy on this same provisioner, with a
// "bridge.invalid" host (RFC 6761 — no resolver answers) so any
// accidental direct-DNS access fails loudly. Production always runs
// in this mode; the device's tunnel transport ignores the host.
//
// In direct deployment (no broker) the URL is the actual host:port
// the runtime is bound to. This path exists so in-cluster operations
// and tests can drive the runtime without a tunnel.
func (p *Provisioner) sandboxURL(id string, port int32) string {
	if p.config.BridgeURL != "" {
		return fmt.Sprintf("http://bridge.invalid/api/sandboxes/%s/runtime", id)
	}
	return fmt.Sprintf("http://%s:%d", p.config.NodeHost, port)
}
