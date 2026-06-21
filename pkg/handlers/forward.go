package handlers

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/agentry/agentry/pkg/tunnel"
)

// ForwardConnectHandler is the terminal hop of the data-plane
// CONNECT chain. The runtime daemon shares a network namespace with
// the user's apps, so `net.Dial("tcp", "127.0.0.1:<port>")` reaches
// whatever the user started.
//
// Request line shape: CONNECT 127.0.0.1:<port> HTTP/1.1
// (Provisioner constructs this from the inbound CONNECT it received.)
//
// HTTP-style /v1/forward/{port}/* is GONE — same job in one fewer
// concept. Anything that speaks TCP (psql, redis, ssh, http, ws, gRPC)
// rides this same path; the runtime never parses the user's bytes.
func ForwardConnectHandler(w http.ResponseWriter, r *http.Request) {
	target := r.RequestURI
	if target == "" {
		target = r.URL.Host
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		http.Error(w, "CONNECT target must be host:port", http.StatusBadRequest)
		return
	}
	// Lock to loopback — defense-in-depth so a misconfigured upstream
	// can't trick the runtime into bridging the sandbox to anywhere on
	// the cluster's bridge network. The provisioner already constrains
	// the target to 127.0.0.1, but we re-check here.
	if host != "127.0.0.1" && host != "localhost" {
		http.Error(w, "CONNECT target host must be loopback", http.StatusForbidden)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	// Block self — the runtime's own HTTP port. Letting a forward
	// target the runtime API would let an outside caller bypass auth
	// by tunneling in raw bytes.
	if port == 8080 {
		http.Error(w, "cannot forward to runtime API port", http.StatusForbidden)
		return
	}

	dial := func() (io.ReadWriteCloser, error) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("dial 127.0.0.1:%d: %w", port, err)
		}
		return c, nil
	}

	inbound, upstream, err := tunnel.AcceptConnect(w, r, dial)
	if err != nil {
		log.Printf("runtime: CONNECT %s: %v", target, err)
		return
	}
	defer inbound.Close()
	defer upstream.Close()
	_ = tunnel.CopyStreams(r.Context(), inbound, upstream, tunnel.CopyOptions{})
}
