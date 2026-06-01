package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/agentry/agentry/pkg/tunnel"
)

// DeployRoute is one hostname → sandbox mapping. The bridge keeps a
// map of these in memory, populated by the control plane via the
// admin API. When a browser request arrives at <hostname>, the bridge
// looks up the target and reverse-proxies through the cluster tunnel
// to the sandbox container's user-app port.
type DeployRoute struct {
	Hostname    string `json:"hostname"`     // e.g. "sales-dash-abc.agentry.live"
	ClusterID   string `json:"cluster_id"`   // matches the cluster-id used at handshake (== cluster name)
	SandboxID   string `json:"sandbox_id"`   // sandbox in that cluster
	Port        int    `json:"port"`         // the port the user's app listens on inside the sandbox
	OrgID       string `json:"org_id"`       // for the future Clerk org-membership check
	AuthMode    string `json:"auth_mode"`    // "org" | "public" — when org, bridge will require an authenticated org member
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

	// Build the upstream path: /api/sandboxes/<sid>/runtime/v1/proxy/<port>/<rest>
	rest := r.URL.Path
	upstreamPath := fmt.Sprintf("/api/sandboxes/%s/runtime/v1/proxy/%d%s",
		route.SandboxID, route.Port, rest)

	originalQuery := r.URL.RawQuery
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "cluster-" + route.ClusterID
			req.URL.Path = upstreamPath
			req.URL.RawQuery = originalQuery
			// Strip cookies + Authorization at this hop so the user's
			// app doesn't see Clerk-session bytes. Slice landing next
			// reintroduces explicit X-Agentry-User-* headers stamped by
			// the bridge Clerk middleware.
			req.Header.Del("Cookie")
			req.Header.Del("Authorization")
		},
		Transport: cluster.rt,
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

// Admin handlers — used by the control plane to push deploy routes.
// Gated by the bridge's existing mTLS gate (anything admin lands on
// HTTPS with a verified client cert from the agentry CA).

type deployRoutesEnvelope struct {
	Routes []DeployRoute `json:"routes"`
}

func (b *Broker) handleDeployRoutesGet(w http.ResponseWriter, _ *http.Request) {
	if b.deploy == nil {
		http.Error(w, "deploy registry not initialized", http.StatusServiceUnavailable)
		return
	}
	_ = writeJSON(w, deployRoutesEnvelope{Routes: b.deploy.All()})
}

func (b *Broker) handleDeployRoutesPut(w http.ResponseWriter, r *http.Request) {
	if b.deploy == nil {
		http.Error(w, "deploy registry not initialized", http.StatusServiceUnavailable)
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
