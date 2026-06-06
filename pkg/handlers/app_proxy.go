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
		ErrorHandler: writeUpstreamUnreachable,
	}
	proxy.ServeHTTP(w, r)
}

// writeUpstreamUnreachable renders a friendly "back in a moment" HTML
// page when the upstream user app isn't responding. Common triggers:
//
//   - The dev server is paused while a deploy build runs (our biggest
//     case — see provisioner.pauseProjectsAt). The dev server will
//     restart in ~30 s; the meta-refresh picks it up automatically.
//   - The user's container crashed and is being auto-restarted by the
//     project manager (exponential backoff up to 16 s).
//   - The deployed container is mid-rollout.
//
// All three resolve in the order of seconds. A meta-refresh of 4 s is
// short enough that the page reloads itself before the user reaches
// for the refresh button, and long enough that a still-down upstream
// doesn't hammer us with retries.
//
// 503 Service Unavailable is the semantically correct status: "server
// temporarily unable to handle the request" — Retry-After=5 is the
// matching hint for any non-browser client (curl, healthchecks) that
// might be hitting the URL.
func writeUpstreamUnreachable(w http.ResponseWriter, _ *http.Request, _ error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	// Inline CSS only — we have no static file plumbing inside the
	// runtime image and don't want the page itself to depend on a
	// network fetch (would compound the failure). The styles match the
	// dashboard's zinc palette so the page feels like part of agentry.
	_, _ = w.Write([]byte(upstreamUnreachableHTML))
}

const upstreamUnreachableHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="4">
<title>Back in a moment</title>
<style>
  html, body { height: 100%; margin: 0; }
  body {
    display: flex; align-items: center; justify-content: center;
    background: #fafafa; color: #18181b;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Inter, system-ui, sans-serif;
    -webkit-font-smoothing: antialiased;
  }
  .card {
    text-align: center; padding: 2rem; max-width: 28rem;
  }
  h1 {
    font-size: 1.5rem; font-weight: 600; letter-spacing: -0.01em;
    margin: 0 0 0.5rem;
  }
  p {
    color: #71717a; font-size: 0.95rem; line-height: 1.5; margin: 0;
  }
  .dot {
    display: inline-block; width: 8px; height: 8px; border-radius: 50%;
    background: #f59e0b; margin-right: 0.5rem; vertical-align: middle;
    animation: pulse 1.4s infinite ease-in-out;
  }
  @keyframes pulse {
    0%, 100% { opacity: 0.4; }
    50% { opacity: 1; }
  }
</style>
</head>
<body>
  <div class="card">
    <h1><span class="dot"></span>Back in a moment</h1>
    <p>This page is refreshing automatically.</p>
  </div>
</body>
</html>`
