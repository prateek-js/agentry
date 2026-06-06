package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/agentry/agentry/pkg/tunnel"
)

// DeployRoute is one hostname → target mapping. The bridge keeps a
// map of these in memory, populated by the control plane via the
// admin API. When a browser request arrives at <hostname>, the bridge
// looks up the target and reverse-proxies through the cluster tunnel.
//
// Kind picks how the upstream URL is built:
//
//   "share"      → sandbox port. Goes through the runtime app_proxy
//                  (/api/sandboxes/<sid>/runtime/v1/proxy/<port>/<rest>).
//                  Live dev process; sandbox-bound.
//   "deployment" → deployment container. Goes through the deployment
//                  proxy (/api/deployments/<dep_id>/proxy/<rest>).
//                  Prod image; survives sandbox deletion.
//
// SandboxID + Port populate when Kind=share. DeploymentID populates
// when Kind=deployment. ClusterID + OrgID + AuthMode apply to both.
type DeployRoute struct {
	Hostname  string `json:"hostname"`           // e.g. "sales-dash-abc.agentry.live"
	Kind      string `json:"kind"`               // "share" | "deployment" (default: "share" for legacy rows)
	ClusterID string `json:"cluster_id"`         // matches the cluster-id used at handshake (== cluster name)
	OrgID     string `json:"org_id"`             // Clerk org gate
	AuthMode  string `json:"auth_mode"`          // "org" | "public"

	// Kind=share fields.
	SandboxID string `json:"sandbox_id,omitempty"`
	Port      int    `json:"port,omitempty"` // the port the user's app listens on inside the sandbox

	// Kind=deployment fields.
	DeploymentID string `json:"deployment_id,omitempty"`
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
