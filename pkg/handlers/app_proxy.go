package handlers

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// AppProxyHandler reverse-proxies into a user-app port inside the
// sandbox container. Path shape:
//
//	/v1/proxy/{port}/{rest...}
//
// rewrites to:
//
//	http://127.0.0.1:{port}/{rest}
//
// Used by the agentry.live deployment URL routing chain:
//
//	browser → bridge ({hostname}.agentry.live)
//	bridge  → cluster tunnel (X-Cluster header)
//	cluster → provisioner runtime_proxy (/api/sandboxes/{sid}/runtime/...)
//	provisioner → runtime container (8080)
//	runtime → user app on 127.0.0.1:{port}  ← THIS HANDLER
//
// All hops above are pre-existing; the only new piece this code adds
// is the last leg from the runtime container to the user's bound port.
//
// Streaming-by-default (httputil.ReverseProxy) so SSE, WebSocket
// upgrade, long-running responses, and large uploads all work.
func AppProxyHandler(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	rest := r.PathValue("rest")
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}

	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		http.Error(w, "bad target", http.StatusInternalServerError)
		return
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = rest
			req.URL.RawQuery = r.URL.RawQuery
			req.Host = target.Host
			// Strip cookies and Authorization at this hop — the user's
			// app shouldn't see Clerk session bytes; the bridge will
			// inject X-Agentry-User-* headers in a later slice.
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "user app on port "+strconv.Itoa(port)+
				" not reachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
