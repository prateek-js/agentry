package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"

	"github.com/agentry-ai/agentry/pkg/tunnel"
)

// DeployRoute is one hostname → target mapping. The bridge keeps a
// map of these in memory, populated by the control plane via the
// admin API. When a browser request arrives at <hostname>, the bridge
// looks up the target and reverse-proxies through the cluster tunnel.
//
// Kind picks how the upstream URL is built:
//
//	"share"      → sandbox port. Goes through the runtime app_proxy
//	               (/api/sandboxes/<sid>/runtime/v1/proxy/<port>/<rest>).
//	               Live dev process; sandbox-bound.
//	"deployment" → deployment container. Goes through the deployment
//	               proxy (/api/deployments/<dep_id>/proxy/<rest>).
//	               Prod image; survives sandbox deletion.
//
// SandboxID + Port populate when Kind=share. DeploymentID populates
// when Kind=deployment. ClusterID + OrgID + AuthMode apply to both.
type DeployRoute struct {
	Hostname  string `json:"hostname"`   // e.g. "sales-dash-abc.agentry.live"
	Kind      string `json:"kind"`       // "share" | "deployment" (default: "share" for legacy rows)
	ClusterID string `json:"cluster_id"` // matches the cluster-id used at handshake (== cluster name)
	OrgID     string `json:"org_id"`     // Clerk org gate
	AuthMode  string `json:"auth_mode"`  // "public" | "org" | "password"

	// Kind=share fields.
	SandboxID string `json:"sandbox_id,omitempty"`
	Port      int    `json:"port,omitempty"` // the port the user's app listens on inside the sandbox

	// Kind=deployment fields.
	DeploymentID string `json:"deployment_id,omitempty"`

	// AuthMode=password fields. Empty/zero for the other auth modes.
	// PasswordHashB64 is the base64-encoded argon2id (salt + key) from
	// agentry-app — verified at the bridge so we don't round-trip per
	// request. PasswordPrefix is the first 8 bytes of the same hash
	// embedded in the unlock cookie so a regenerate (which produces a
	// different hash and a different prefix) strictly invalidates
	// every cookie minted under the old passphrase.
	PasswordHashB64 string `json:"password_hash_b64,omitempty"`
	PasswordPrefix  uint64 `json:"password_prefix,omitempty"`
}

// DeployRegistry is the in-memory hostname → route map. Mutated by
// the admin API and the control-plane sync; read on every browser
// request, so we keep the hot path lock-free with an atomic snapshot.
type DeployRegistry struct {
	mu     sync.RWMutex
	routes map[string]DeployRoute // key = hostname (lowercase)
}

// NewDeployRegistry returns an empty registry.
func NewDeployRegistry() *DeployRegistry {
	return &DeployRegistry{routes: make(map[string]DeployRoute)}
}

// Lookup returns the route for a hostname, or false if none.
func (r *DeployRegistry) Lookup(hostname string) (DeployRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.routes[strings.ToLower(hostname)]
	return v, ok
}

// Set inserts or replaces a route for hostname.
func (r *DeployRegistry) Set(route DeployRoute) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[strings.ToLower(route.Hostname)] = route
}

// Delete removes a route by hostname; no-op when missing.
func (r *DeployRegistry) Delete(hostname string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, strings.ToLower(hostname))
}

// ReplaceAll atomically swaps in a fresh routes map. Used by the
// control-plane sync to keep the bridge's view authoritative without
// per-row delta tracking.
func (r *DeployRegistry) ReplaceAll(routes []DeployRoute) {
	next := make(map[string]DeployRoute, len(routes))
	for _, r := range routes {
		next[strings.ToLower(r.Hostname)] = r
	}
	r.mu.Lock()
	r.routes = next
	r.mu.Unlock()
}

// All returns a snapshot of every route. Used by the admin debug
// endpoint and by tests.
func (r *DeployRegistry) All() []DeployRoute {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]DeployRoute, 0, len(r.routes))
	for _, v := range r.routes {
		out = append(out, v)
	}
	return out
}

// AttachDeploy wires the registry into the Broker. The Broker stays
// the source of truth for cluster sessions; the registry just adds
// the hostname → (cluster, sandbox, port) layer on top.
func (b *Broker) AttachDeploy(reg *DeployRegistry) {
	b.deploy = reg
}

// HandleDeployment is the entry point for *.agentry.live (or whatever
// deployDomain the bridge is configured for). Called from cmd/bridge
// when r.Host is under the deploy domain. Returns 404 when the
// hostname isn't registered; 502 when the cluster isn't connected.
func (b *Broker) HandleDeployment(w http.ResponseWriter, r *http.Request) {
	if b.deploy == nil {
		http.Error(w, "deploy routing not configured on this bridge", http.StatusServiceUnavailable)
		return
	}
	host := r.Host
	if i := strings.Index(host, ":"); i > 0 {
		host = host[:i]
	}
	route, ok := b.deploy.Lookup(host)
	if !ok {
		// Falls through to the placeholder page on the cmd/bridge side
		// so the user sees something nice instead of a bare 404. The
		// cmd/bridge dispatcher calls this function only after it has
		// already decided this is a deploy-domain request.
		http.NotFound(w, r)
		return
	}

	b.mu.RLock()
	cluster := b.clusters[route.ClusterID]
	b.mu.RUnlock()
	if cluster == nil {
		http.Error(w, fmt.Sprintf("cluster %q is offline", route.ClusterID), http.StatusBadGateway)
		return
	}
	// Defense in depth. route.OrgID is stamped by the control plane;
	// cluster.orgID comes from the cluster's cert URI SAN at handshake.
	// They MUST agree — a deployment in org A can only route through a
	// cluster in org A. If they disagree, something is wrong upstream
	// (corrupted route push, mis-issued cert, attempted lateral move
	// across orgs). Fail closed and log loudly: the alternative is
	// serving one org's deployment traffic out of another org's cluster,
	// which is the worst class of cross-tenant bug. Skip the check in
	// DevMode because cluster.orgID is unset there.
	if !b.cfg.DevMode && route.OrgID != "" && route.OrgID != cluster.orgID {
		log.Printf("bridge: cross-org deploy route refused: host=%s route.org=%s cluster=%s cluster.org=%s",
			host, route.OrgID, route.ClusterID, cluster.orgID)
		http.Error(w, "deployment route is inconsistent with cluster ownership",
			http.StatusBadGateway)
		return
	}

	// Build the upstream path based on Kind:
	//   share      → /api/sandboxes/<sid>/runtime/v1/proxy/<port>/<rest>
	//   deployment → /api/deployments/<dep_id>/proxy/<rest>
	// Legacy rows (no Kind set) default to share for back-compat with
	// any old in-memory state that pre-dates the field.
	rest := r.URL.Path
	var upstreamPath string
	switch route.Kind {
	case "deployment":
		if route.DeploymentID == "" {
			http.Error(w, "deployment route missing deployment_id", http.StatusInternalServerError)
			return
		}
		upstreamPath = fmt.Sprintf("/api/deployments/%s/proxy%s", route.DeploymentID, rest)
	default: // "share" or empty
		if route.SandboxID == "" || route.Port == 0 {
			http.Error(w, "share route missing sandbox_id/port", http.StatusInternalServerError)
			return
		}
		upstreamPath = fmt.Sprintf("/api/sandboxes/%s/runtime/v1/proxy/%d%s",
			route.SandboxID, route.Port, rest)
	}

	originalQuery := r.URL.RawQuery
	// "Built with agentry" badge: injected only on PUBLIC dev-preview
	// shares (the ephemeral *.agentry.live links people pass around).
	// Never on deployments, never on org/password shares, never on
	// custom domains (those reach HandleDeployment only via a registered
	// route, but the public-share gate below still excludes them).
	injectBadge := route.Kind != "deployment" && strings.EqualFold(route.AuthMode, "public")
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// For pages we rewrite, ask upstream for uncompressed HTML so
			// ModifyResponse can splice the badge without gzip round-trips.
			if injectBadge {
				req.Header.Set("Accept-Encoding", "identity")
			}
			// Preserve the public hostname for downstream consumers.
			// The runtime's app_proxy clobbers Host to 127.0.0.1:PORT
			// when it forwards into the sandbox, so any handler past
			// that point (notably the in-sandbox authproxy sidecar's
			// same-origin check) needs to read X-Forwarded-Host to know
			// the original URL the browser saw. Set this BEFORE we
			// rewrite the URL.
			if req.Header.Get("X-Forwarded-Host") == "" {
				req.Header.Set("X-Forwarded-Host", req.Host)
			}
			if req.Header.Get("X-Forwarded-Proto") == "" {
				req.Header.Set("X-Forwarded-Proto", "https")
			}
			req.URL.Scheme = "http"
			req.URL.Host = "cluster-" + route.ClusterID
			req.URL.Path = upstreamPath
			req.URL.RawQuery = originalQuery
			// Selectively strip Clerk dashboard cookies at this hop so
			// a user logged into app.agentry.run doesn't ship Clerk
			// session bytes into a sandbox app. Everything else passes
			// through — agentry_csrf and agentry_session belong to the
			// in-sandbox authproxy sidecar, and dropping them broke
			// sign-in (see m2 Bug C: blanket Cookie strip caused
			// "CSRF cookie missing" 403 on every preview/deploy URL).
			if filtered := filterClerkCookies(req.Header.Get("Cookie")); filtered != "" {
				req.Header.Set("Cookie", filtered)
			} else {
				req.Header.Del("Cookie")
			}
			// Authorization is always Clerk-issued (Bearer JWT), so a
			// blanket drop is still correct.
			req.Header.Del("Authorization")
		},
		Transport: cluster.rt,
		ModifyResponse: func(resp *http.Response) error {
			if injectBadge {
				injectBuiltWithBadge(resp)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, tunnel.ErrSessionClosed) {
				http.Error(w, "cluster session closed mid-request", http.StatusBadGateway)
				return
			}
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// builtWithBadge is the floating "Built with agentry" pill spliced into
// public preview pages. Self-contained inline styles + a max z-index so
// it survives whatever CSS the app ships; opens agentry.run in a new tab.
const builtWithBadge = `<div style="position:fixed;bottom:12px;right:12px;z-index:2147483647;font-family:system-ui,-apple-system,sans-serif">` +
	`<a href="https://agentry.run?utm_source=preview_badge" target="_blank" rel="noopener noreferrer" ` +
	`style="display:inline-flex;align-items:center;gap:6px;background:#18181b;color:#fff;text-decoration:none;` +
	`font-size:12px;font-weight:500;line-height:1;padding:6px 12px;border-radius:9999px;box-shadow:0 4px 14px rgba(0,0,0,.18)">` +
	`<span style="width:7px;height:7px;border-radius:9999px;background:#10b981"></span>Built with agentry</a></div>`

// injectBuiltWithBadge splices the badge into an HTML response, just
// before </body>. No-op unless the response is a 2xx text/html page that
// fits a sane size cap. Conservative by design: anything unusual (non-HTML,
// already-compressed, oversized, read error) is left exactly as-is so we
// never corrupt a user's app.
func injectBuiltWithBadge(resp *http.Response) {
	if resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return // still compressed despite our identity hint — don't touch
	}
	const maxInject = 8 << 20 // 8 MiB cap; bigger pages stream through untouched
	if resp.ContentLength > maxInject {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInject+1))
	_ = resp.Body.Close()
	if err != nil || int64(len(body)) > maxInject {
		// Read failed or over cap — restore what we have, unmodified.
		resp.Body = io.NopCloser(bytes.NewReader(body))
		if err == nil {
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		}
		return
	}
	// Insert before the last </body> (case-insensitive); append if absent.
	out := body
	if i := lastIndexFold(body, []byte("</body>")); i >= 0 {
		out = append(append(append([]byte{}, body[:i]...), []byte(builtWithBadge)...), body[i:]...)
	} else {
		out = append(append([]byte{}, body...), []byte(builtWithBadge)...)
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
}

// lastIndexFold is bytes.LastIndex with ASCII case-insensitivity for the
// needle — enough for matching </body> / </BODY> / </Body>.
func lastIndexFold(haystack, needle []byte) int {
	return bytes.LastIndex(bytes.ToLower(haystack), bytes.ToLower(needle))
}

// filterClerkCookies removes Clerk-issued dashboard cookies from a
// raw Cookie header value, returning the surviving cookies as a
// re-joined header. Empty string means "no surviving cookies" — the
// caller deletes the Cookie header entirely.
//
// We strip:
//   - __session         — Clerk's primary session cookie
//   - __client_uat      — Clerk's last-active timestamp
//   - __client_*        — every Clerk client-prefixed cookie
//
// Everything else passes through, including the in-sandbox authproxy
// sidecar's agentry_csrf + agentry_session cookies (without which
// sign-in is impossible — see m2 Bug C).
//
// Parsing rule: cookies in a request header are name=value pairs
// separated by "; " per RFC 6265. We split on "; ", trim, and check
// the name half. Malformed entries (no "=") are dropped silently
// because that's what every other HTTP client does — we're not in
// the validation business here, just the filtering one.
func filterClerkCookies(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		eq := strings.IndexByte(p, '=')
		var name string
		if eq < 0 {
			name = p
		} else {
			name = p[:eq]
		}
		if isClerkCookieName(name) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "; ")
}

// isClerkCookieName matches the Clerk cookie set. Centralised so a
// future Clerk SDK update lands as a one-line change here. Note: the
// names are case-sensitive per RFC 6265, and Clerk always emits the
// double-underscore prefix.
func isClerkCookieName(name string) bool {
	switch name {
	case "__session", "__client_uat":
		return true
	}
	return strings.HasPrefix(name, "__client_")
}

// Admin handlers — used by the control plane to push deploy routes.
// Gated by the bridge's existing mTLS gate (anything admin lands on
// HTTPS with a verified client cert from the agentry CA).

type deployRoutesEnvelope struct {
	Routes []DeployRoute `json:"routes"`
}

// requireAdmin gates an admin-only endpoint. The bridge's mTLS layer
// requires SOME valid cert; this is the second gate that distinguishes
// admin certs (URI SAN = urn:agentry:admin) from regular device certs
// (URI SAN = urn:agentry:org:<id>). Without this gate, any signed-in
// user with a device cert could ReplaceAll the route table — wiping
// every other org's deploy URLs or pointing them at attacker clusters.
//
// DevMode skips the check (local-loop testing has no certs).
//
// Returns true when the request is allowed to proceed; on false the
// handler has already written a 403 response.
func (b *Broker) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if b.cfg.DevMode {
		return true
	}
	_, admin := peerOrgIdentity(r)
	if !admin {
		http.Error(w, "admin cert required for this endpoint", http.StatusForbidden)
		return false
	}
	return true
}

func (b *Broker) handleDeployRoutesGet(w http.ResponseWriter, r *http.Request) {
	if b.deploy == nil {
		http.Error(w, "deploy registry not initialized", http.StatusServiceUnavailable)
		return
	}
	if !b.requireAdmin(w, r) {
		return
	}
	_ = writeJSON(w, deployRoutesEnvelope{Routes: b.deploy.All()})
}

func (b *Broker) handleDeployRoutesPut(w http.ResponseWriter, r *http.Request) {
	if b.deploy == nil {
		http.Error(w, "deploy registry not initialized", http.StatusServiceUnavailable)
		return
	}
	if !b.requireAdmin(w, r) {
		return
	}
	var body deployRoutesEnvelope
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	b.deploy.ReplaceAll(body.Routes)
	w.WriteHeader(http.StatusNoContent)
}
