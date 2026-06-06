package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/agentry/agentry/pkg/tunnel"
	"github.com/hashicorp/yamux"
)

// HeaderDeviceID and HeaderClusterID are protocol-level handshake
// headers, re-exported from pkg/tunnel so callers reading bridge.go
// don't have to chase the definition. Kept as aliases (not duplicate
// const declarations) so there's exactly one source of truth.
const (
	HeaderDeviceID  = tunnel.HeaderDeviceID
	HeaderClusterID = tunnel.HeaderClusterID
)

// HeaderTargetCluster is the request-time header devices send on each
// MCP call to tell the bridge which cluster to forward to. This one
// is bridge-internal — the cluster on the other side never sees it
// (the bridge strips it before forwarding).
const HeaderTargetCluster = "X-Cluster"

// Config holds the bridge's runtime settings. The bridge is a
// stateless routing pivot — cert issuance, identity validation, and
// deploy bookkeeping all live in the control plane (app.agentry.run).
// What's left here is purely "should we run mTLS or plain HTTP" and
// "where do we live".
type Config struct {
	// DevMode disables mTLS enforcement on /tunnel + /api/*. Plain HTTP
	// listener, no client cert required, no role-CN binding. Strictly
	// for local-loop testing; ALWAYS false in production. Set via
	// DEV_MODE=1 on the bridge binary.
	DevMode bool
}

// Broker owns the device + cluster session directories and the
// device → cluster routing decision. One Broker per process.
//
// Safe for concurrent use. All session-table mutations go through mu;
// reads are RLock so the hot routing path doesn't contend with rare
// connect/disconnect events.
type Broker struct {
	cfg Config
	mu  sync.RWMutex
	// devices: device CN → list of currently-active yamux sessions.
	// Multiple sessions per CN are allowed on purpose: Roo, Claude
	// Code, Cursor each spawn their own `agentry stdio`, and Roo will
	// respawn the child on any exit. Booting the older session every
	// time a new one connects (the previous behaviour) turned that
	// into a tunnel firehose where every in-flight request raced a
	// kick. Routes pick the newest live session per CN.
	devices  map[string][]*deviceConn
	clusters map[string]*clusterConn

	// deploy is the optional hostname → sandbox-port registry. Set via
	// AttachDeploy from cmd/bridge after the registry exists. Nil when
	// the bridge isn't configured for deployment ingress.
	deploy *DeployRegistry
}

// Tenancy: every connected device + cluster carries the org_id that
// agentry-app stamped on its cert URI SAN at issuance. The bridge
// pulls these out of the peer cert during handleTunnel and refuses
// to route a request from device-org-X to cluster-org-Y when X != Y.
// Admin certs (the syncer) carry urn:agentry:admin and bypass the
// filter.

type deviceConn struct {
	id        string
	orgID     string // urn:agentry:org:<id> from device cert SAN
	admin     bool   // urn:agentry:admin from device cert SAN (syncer only)
	sess      *yamux.Session
	connected time.Time
}

type clusterConn struct {
	id        string
	orgID     string // urn:agentry:org:<id> from cluster cert SAN
	sess      *yamux.Session
	rt        *tunnel.RoundTripper
	connected time.Time
}

// New returns an empty Broker with no special config. Equivalent to
// NewWithConfig(Config{}) — auth-disabled core router only.
func New() *Broker { return NewWithConfig(Config{}) }

// NewWithConfig returns an empty Broker. cfg.DevMode is the only
// runtime knob; everything else (which CA we trust, where we listen,
// which domain we serve) is settled by cmd/bridge from env vars +
// TLS config it builds outside this package.
func NewWithConfig(cfg Config) *Broker {
	return &Broker{
		cfg:      cfg,
		devices:  make(map[string][]*deviceConn),
		clusters: make(map[string]*clusterConn),
	}
}

// Handler returns the http.Handler the bridge exposes on its listen
// address. Three routes — anything else is a 404 by design. Cert
// issuance + management endpoints live in the control plane, not
// here.
//
//	PUT  /tunnel        — handshake endpoint (devices and clusters)
//	GET  /api/clusters  — connection census for ops (mTLS-gated in prod)
//	GET  /healthz       — liveness for load balancers
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /tunnel", b.handleTunnel)
	mux.HandleFunc("GET /api/clusters", b.handleClustersList)
	mux.HandleFunc("GET /api/clusters/{id}/sandboxes", b.handleClusterSandboxes)
	mux.HandleFunc("GET /api/clusters/{id}/sandboxes/{sid}", b.handleClusterSandboxGet)
	mux.HandleFunc("DELETE /api/clusters/{id}/sandboxes/{sid}", b.handleClusterSandboxDelete)
	mux.HandleFunc("POST /api/clusters/{id}/sandboxes/{sid}/renew", b.handleClusterSandboxRenew)
	mux.HandleFunc("/api/clusters/{id}/sandboxes/{sid}/runtime/{rest...}", b.handleClusterSandboxRuntime)
	// Generic deploy-build + deployment lifecycle proxy. Same shape as
	// the runtime wildcard above, but the cluster-side path lives under
	// /api/deployments (deploy run/get/stop) and /api/sandboxes/{sid}/
	// deploy-build. Used by the deployment orchestrator on agentry-app.
	mux.HandleFunc("/api/clusters/{id}/sandboxes/{sid}/deploy-build", b.handleClusterDeployBuild)
	mux.HandleFunc("/api/clusters/{id}/sandboxes/{sid}/deploy-push", b.handleClusterDeployPush)
	// Bindings: GET ".../sandboxes/{sid}/bindings" returns names + source
	// services only (no values); GET ".../bindings/env" is the privileged
	// resolved-env path used by the control plane at deploy time. Both
	// proxy through to the provisioner verbatim.
	mux.HandleFunc("GET /api/clusters/{id}/sandboxes/{sid}/bindings", b.handleClusterSandboxBindings)
	mux.HandleFunc("GET /api/clusters/{id}/sandboxes/{sid}/bindings/env", b.handleClusterSandboxBindingsEnv)
	// User-staged per-sandbox secrets. GET returns names only; POST
	// writes a value the runtime exports on next shell start.
	mux.HandleFunc("GET /api/clusters/{id}/sandboxes/{sid}/secrets", b.handleClusterSandboxSecretsList)
	mux.HandleFunc("POST /api/clusters/{id}/sandboxes/{sid}/secrets", b.handleClusterSandboxSecretSet)
	mux.HandleFunc("/api/clusters/{id}/deployments", b.handleClusterDeploymentsRoot)
	mux.HandleFunc("/api/clusters/{id}/deployments/{rest...}", b.handleClusterDeployments)
	mux.HandleFunc("GET /api/deploy-routes", b.handleDeployRoutesGet)
	mux.HandleFunc("PUT /api/deploy-routes", b.handleDeployRoutesPut)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// handleTunnel is the dispatch point for both device and cluster
// handshakes. Reads the role header, runs tunnel.Accept, then routes
// to the per-role serve loop.
//
// In prod mode (the cmd/bridge mTLS middleware passed us a verified
// peer cert), we ALSO enforce that the cert's CN matches the
// requested role: a device cert can't register as a cluster, and
// vice versa. This closes the "valid laptop tries to take over a
// cluster slot" gap.
func (b *Broker) handleTunnel(w http.ResponseWriter, r *http.Request) {
	role := tunnel.Role(r.Header.Get(tunnel.HandshakeHeader))
	switch role {
	case tunnel.RoleDevice:
		if !b.cfg.DevMode {
			if !peerCNHasPrefix(r, "device-") {
				http.Error(w, "device role requires a cert with CN device-*", http.StatusForbidden)
				return
			}
		}
		deviceID := r.Header.Get(HeaderDeviceID)
		if deviceID == "" {
			http.Error(w, "missing "+HeaderDeviceID, http.StatusBadRequest)
			return
		}
		// Tenancy: read the org_id (or admin flag) out of the device
		// cert URI SAN. agentry-app stamps these at issuance; without
		// them the request can't route anywhere.
		orgID, admin := peerOrgIdentity(r)
		if !b.cfg.DevMode && !admin && orgID == "" {
			http.Error(w, "device cert missing org SAN", http.StatusForbidden)
			return
		}
		sess, err := tunnel.Accept(w, r, tunnel.AcceptConfig{})
		if err != nil {
			log.Printf("bridge: device accept failed: %v", err)
			return
		}
		b.serveDevice(deviceID, orgID, admin, sess)

	case tunnel.RoleCluster:
		clusterID := r.Header.Get(HeaderClusterID)
		if clusterID == "" {
			http.Error(w, "missing "+HeaderClusterID, http.StatusBadRequest)
			return
		}
		if !b.cfg.DevMode {
			// In prod, the cert's CN must be exactly "cluster-<id>"
			// — same identifier the provisioner declared at handshake.
			// Catches both wrong-role (device-*) and wrong-cluster
			// (cluster-someone-elses) attempts.
			wantCN := "cluster-" + clusterID
			if !peerCNEquals(r, wantCN) {
				http.Error(w, fmt.Sprintf("cluster role requires a cert with CN %q", wantCN),
					http.StatusForbidden)
				return
			}
		}
		// Tenancy: a cluster cert carries its owning org_id in the URI
		// SAN. Without it the bridge would have nothing to compare
		// against when a device asked to route here.
		orgID, _ := peerOrgIdentity(r)
		if !b.cfg.DevMode && orgID == "" {
			http.Error(w, "cluster cert missing org SAN", http.StatusForbidden)
			return
		}
		sess, err := tunnel.Accept(w, r, tunnel.AcceptConfig{})
		if err != nil {
			log.Printf("bridge: cluster accept failed: %v", err)
			return
		}
		b.serveCluster(clusterID, orgID, sess)

	default:
		http.Error(w, "unknown or missing "+tunnel.HandshakeHeader, http.StatusBadRequest)
	}
}

// peerOrgIdentity pulls the URI SAN out of the request's verified peer
// cert and returns (org_id, admin). The SAN scheme is:
//
//	urn:agentry:org:<id>   for orgs
//	urn:agentry:admin      for the agentry-app syncer cert
//
// We return (orgID="", admin=false) if the request has no peer cert
// (dev mode), no URIs in the SAN (legacy cert), or an unknown scheme.
// Callers decide what to do with that absence; in prod handleTunnel
// rejects it.
func peerOrgIdentity(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", false
	}
	const orgPrefix = "urn:agentry:org:"
	const adminURN = "urn:agentry:admin"
	for _, u := range r.TLS.PeerCertificates[0].URIs {
		s := u.String()
		if s == adminURN {
			return "", true
		}
		if len(s) > len(orgPrefix) && s[:len(orgPrefix)] == orgPrefix {
			return s[len(orgPrefix):], false
		}
	}
	return "", false
}

// handleClusterSandboxes proxies a GET /api/sandboxes request to the
// named cluster's provisioner through its open tunnel session. Used by
// the control plane's dashboard to render "what's running in this
// cluster" without standing up a separate device-style tunnel from the
// control plane.
//
// Same mTLS gate as /api/clusters — the cmd/bridge wrapper enforces a
// valid client cert before the request reaches this handler in prod.
//
// If the cluster isn't currently connected, returns 502 with a clear
// message (the dashboard renders this as "cluster offline" instead of
// a generic error).
func (b *Broker) handleClusterSandboxes(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"), "/api/sandboxes")
}

// handleClusterSandboxRuntime is the generic wildcard proxy for any
// HTTP call the dashboard wants to make against a running sandbox's
// runtime container (process list, port list, project status, file
// tree, …). Path shape:
//
//	/api/clusters/{id}/sandboxes/{sid}/runtime/{rest...}
//
// gets rewritten to the cluster-side path the provisioner already
// terminates:
//
//	/api/sandboxes/{sid}/runtime/{rest}
//
// from there the provisioner's existing runtime_proxy hops one more
// level inside the sandbox network. Same mTLS gate + same tunnel as
// the list/delete endpoints; method, body, query string flow through.
func (b *Broker) handleClusterSandboxRuntime(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sid := r.PathValue("sid")
	rest := r.PathValue("rest")
	b.proxyToCluster(w, r, id, "/api/sandboxes/"+sid+"/runtime/"+rest)
}

// handleClusterSandboxDelete proxies a DELETE /api/sandboxes/{sid} to
// the cluster's provisioner. Same gate + same tunnel as the list
// endpoint; the only thing that varies is the upstream path.
func (b *Broker) handleClusterSandboxDelete(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"), "/api/sandboxes/"+r.PathValue("sid"))
}

// handleClusterSandboxGet proxies a single-sandbox lookup. Returns
// SandboxInfo (sandbox_id, sandbox_url, status, expires_at) — used by
// the dashboard detail page to surface the expiry timestamp and
// enable the "Extend" affordance.
func (b *Broker) handleClusterSandboxGet(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"), "/api/sandboxes/"+r.PathValue("sid"))
}

// handleClusterSandboxRenew pushes the sandbox's reaper deadline out
// by ttl_seconds (taken from the body). The provisioner reuses the
// prior TTL if the body omits ttl_seconds, but our control plane
// always sends an explicit value so the user's intent is unambiguous.
func (b *Broker) handleClusterSandboxRenew(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"),
		"/api/sandboxes/"+r.PathValue("sid")+"/renew")
}

// handleClusterDeployBuild proxies the deploy-build call into the
// sandbox's provisioner. Used by the deployment orchestrator to kick
// off image build inside the source sandbox.
func (b *Broker) handleClusterDeployBuild(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"),
		"/api/sandboxes/"+r.PathValue("sid")+"/deploy-build")
}

// handleClusterDeployPush proxies the deploy-push call. Body carries
// a decrypted registry token in transit — the bridge sees the bytes
// but doesn't parse them, and the tunnel underneath is mTLS-gated.
func (b *Broker) handleClusterDeployPush(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"),
		"/api/sandboxes/"+r.PathValue("sid")+"/deploy-push")
}

// handleClusterSandboxBindings proxies the "what's bound on this
// sandbox" call to the provisioner. Returns service → env-var-names
// only (no credential values). The dashboard's deploy form calls this
// to render the inheritance checklist.
func (b *Broker) handleClusterSandboxBindings(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"),
		"/api/sandboxes/"+r.PathValue("sid")+"/bindings")
}

// handleClusterSandboxBindingsEnv proxies the privileged "resolved env
// for this sandbox" call. Returns full key→value map. Called by the
// control plane's deploy orchestrator only.
func (b *Broker) handleClusterSandboxBindingsEnv(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"),
		"/api/sandboxes/"+r.PathValue("sid")+"/bindings/env")
}

// handleClusterSandboxSecretsList proxies the GET names-only secrets
// list. Powers the dashboard's per-sandbox "Secrets" section so the
// user can see what's staged without ever revealing values.
func (b *Broker) handleClusterSandboxSecretsList(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"),
		"/api/sandboxes/"+r.PathValue("sid")+"/secrets")
}

// handleClusterSandboxSecretSet proxies POST with {name, value}.
// The provisioner writes the value to /var/run/agentry/secrets/<NAME>
// inside the sandbox; the shell shim exports it on next process start.
// agentry-app forwards body unchanged — values never appear in bridge
// logs because the proxy is a byte-stream forward (no JSON parse here).
func (b *Broker) handleClusterSandboxSecretSet(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"),
		"/api/sandboxes/"+r.PathValue("sid")+"/secrets")
}

// handleClusterDeployments{,Root} proxy deployment lifecycle calls
// (run / get / stop) to the cluster's provisioner. Split into two
// handlers because Go's path patterns can't distinguish ".../deployments"
// from ".../deployments/{rest...}" on POST in one pattern.
func (b *Broker) handleClusterDeploymentsRoot(w http.ResponseWriter, r *http.Request) {
	b.proxyToCluster(w, r, r.PathValue("id"), "/api/deployments")
}

func (b *Broker) handleClusterDeployments(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if rest == "" {
		b.proxyToCluster(w, r, r.PathValue("id"), "/api/deployments")
		return
	}
	b.proxyToCluster(w, r, r.PathValue("id"), "/api/deployments/"+rest)
}

// proxyToCluster is the shared reverse-proxy plumbing for any admin
// endpoint that pivots a single HTTP request through a connected
// cluster's tunnel. Method + body + headers all flow verbatim; only
// the URL path is rewritten to the cluster's API shape.
func (b *Broker) proxyToCluster(w http.ResponseWriter, r *http.Request, id, upstreamPath string) {
	// Tenant enforcement: the requester (admin syncer or org-scoped
	// device cert) must own the target cluster. We resolve the
	// requester's org from the peer cert, then look up the cluster
	// under (org, name). Non-matches are 404, NOT 403 — we never
	// confirm that a cluster name exists in another org.
	reqOrg, reqAdmin := peerOrgIdentity(r)
	b.mu.RLock()
	cluster := b.clusters[id]
	b.mu.RUnlock()
	if cluster == nil || (!reqAdmin && !b.cfg.DevMode && cluster.orgID != reqOrg) {
		http.Error(w, fmt.Sprintf("cluster %q is offline", id), http.StatusBadGateway)
		return
	}
	originalQuery := r.URL.RawQuery
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "cluster-" + id
			req.URL.Path = upstreamPath
			// Preserve query string — runtime endpoints (and any future
			// admin route with filters) need it. Bridge-internal list /
			// delete calls don't pass one, so this is a no-op for them.
			req.URL.RawQuery = originalQuery
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

// handleClustersList returns the connected-cluster census, scoped to
// the requester's org. Admin certs (the agentry-app syncer) see every
// cluster; everyone else sees only the clusters their org owns. This
// is the endpoint that used to leak — the old version returned every
// cluster on the bridge regardless of peer identity.
func (b *Broker) handleClustersList(w http.ResponseWriter, r *http.Request) {
	reqOrg, reqAdmin := peerOrgIdentity(r)
	type clusterInfo struct {
		ID           string `json:"id"`
		Connected    bool   `json:"connected"`
		ConnectedAgo string `json:"connected_ago,omitempty"`
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	now := time.Now()
	out := struct {
		Clusters []clusterInfo `json:"clusters"`
	}{Clusters: make([]clusterInfo, 0, len(b.clusters))}
	for _, c := range b.clusters {
		// Dev mode is unauthenticated by definition — show everything.
		// In prod, admin sees all; everyone else sees their org only.
		if !b.cfg.DevMode && !reqAdmin && c.orgID != reqOrg {
			continue
		}
		out.Clusters = append(out.Clusters, clusterInfo{
			ID:           c.id,
			Connected:    true,
			ConnectedAgo: now.Sub(c.connected).Truncate(1e9).String(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = writeJSON(w, out)
}

// peerCNHasPrefix returns true when the verified peer cert's CN
// starts with the given prefix. Used for the device-role check
// where we only need "any device cert".
func peerCNHasPrefix(r *http.Request, prefix string) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	cn := r.TLS.PeerCertificates[0].Subject.CommonName
	if len(cn) < len(prefix) {
		return false
	}
	return cn[:len(prefix)] == prefix
}

// peerCNEquals returns true when the verified peer cert's CN matches
// the expected value exactly. Used for cluster registration where the
// cert identity has to line up with the requested cluster name.
func peerCNEquals(r *http.Request, want string) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return false
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName == want
}

// serveDevice runs the inner HTTP server over the device's yamux
// session. Each accepted yamux stream is treated as one HTTP request
// from the device; the request handler reads X-Cluster and reverse-
// proxies to the matching cluster session.
//
// Blocks until the session closes, then removes the device from the
// directory.
func (b *Broker) serveDevice(id, orgID string, admin bool, sess *yamux.Session) {
	dc := &deviceConn{id: id, orgID: orgID, admin: admin, sess: sess, connected: time.Now()}
	b.mu.Lock()
	b.devices[id] = append(b.devices[id], dc)
	n := len(b.devices[id])
	b.mu.Unlock()
	if n == 1 {
		log.Printf("bridge: device %s connected", id)
	} else {
		// Second+ live session for the same CN — the user has Roo and
		// Claude Code (etc.) both running, or Roo's child is racing a
		// respawn. Note it once per attach instead of kicking the old.
		log.Printf("bridge: device %s additional session attached (now %d live)", id, n)
	}

	defer func() {
		b.mu.Lock()
		// Remove THIS session from the slice; leave any siblings alone.
		list := b.devices[id]
		for i, cur := range list {
			if cur == dc {
				b.devices[id] = append(list[:i], list[i+1:]...)
				break
			}
		}
		remaining := len(b.devices[id])
		if remaining == 0 {
			delete(b.devices, id)
		}
		b.mu.Unlock()
		if remaining == 0 {
			log.Printf("bridge: device %s disconnected", id)
		} else {
			log.Printf("bridge: device %s session detached (%d still live)", id, remaining)
		}
	}()

	srv := &http.Server{
		Handler:           b.deviceHandler(id),
		ReadHeaderTimeout: 30 * time.Second,
	}
	// http.Serve runs until the listener (yamux session) errors —
	// which it does on session close. Both ErrSessionShutdown and
	// EOF are normal shapes for "the peer closed cleanly"; only log
	// what's unexpected.
	if err := srv.Serve(sess); err != nil && !isCleanSessionEnd(err) {
		log.Printf("bridge: device %s serve: %v", id, err)
	}
}

// isCleanSessionEnd matches the error shapes yamux returns when the
// session closes for normal reasons (peer hung up, we initiated
// shutdown). Keeps the bridge log free of one-line-per-disconnect
// noise while still surfacing genuine protocol errors.
func isCleanSessionEnd(err error) bool {
	if err == nil ||
		errors.Is(err, yamux.ErrSessionShutdown) ||
		errors.Is(err, io.EOF) {
		return true
	}
	s := err.Error()
	return s == "use of closed network connection"
}

// serveCluster registers the cluster's session and blocks until it
// closes. The bridge doesn't accept streams from the cluster — it
// opens them on demand from device handlers — so all this loop has
// to do is hold the registration alive.
func (b *Broker) serveCluster(id, orgID string, sess *yamux.Session) {
	rt := tunnel.NewRoundTripper(sess)
	cc := &clusterConn{id: id, orgID: orgID, sess: sess, rt: rt, connected: time.Now()}
	b.mu.Lock()
	if old, ok := b.clusters[id]; ok {
		_ = old.sess.Close()
	}
	b.clusters[id] = cc
	b.mu.Unlock()
	log.Printf("bridge: cluster %s connected", id)

	defer func() {
		b.mu.Lock()
		if cur, ok := b.clusters[id]; ok && cur == cc {
			delete(b.clusters, id)
		}
		b.mu.Unlock()
		log.Printf("bridge: cluster %s disconnected", id)
	}()

	<-sess.CloseChan()
}

// deviceHandler is the http.Handler that fronts a connected device's
// inner HTTP server. Every accepted yamux stream from the device
// drops one request into this handler; the handler demuxes on method:
//
//	CONNECT  → byte-pipe data plane (forward, psql, ssh, …)
//	others   → HTTP reverse-proxy for MCP RPC + provisioner API
//
// Both paths route on X-Cluster; the device-id is stamped through as
// X-Forwarded-Device so the cluster knows who originated the request.
func (b *Broker) deviceHandler(deviceID string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := r.Header.Get(HeaderTargetCluster)
		if target == "" {
			http.Error(w, "missing "+HeaderTargetCluster, http.StatusBadRequest)
			return
		}
		b.mu.RLock()
		cluster := b.clusters[target]
		b.mu.RUnlock()
		if cluster == nil {
			http.Error(w, fmt.Sprintf("cluster %q is offline", target), http.StatusBadGateway)
			return
		}

		if r.Method == http.MethodConnect {
			b.handleDeviceConnect(w, r, deviceID, cluster)
			return
		}

		// HTTP path — ReverseProxy with our yamux RoundTripper.
		// Director rewrites the URL to something http.Request.Write
		// accepts; the actual destination ignores it (every request
		// on a yamux session lands at the peer regardless of URL).
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = "cluster-" + target
				req.Header.Del(HeaderTargetCluster)
				req.Header.Set("X-Forwarded-Device", deviceID)
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
	})
}

// handleDeviceConnect handles a CONNECT from the device side. It
// opens a fresh yamux stream on the cluster session, replays the
// CONNECT to the cluster (with a verified X-Forwarded-Device and
// without the bridge-internal X-Cluster), waits for the cluster's
// 200, then byte-pipes the two streams together.
//
// The target in the request line (sandbox:port) flows through as-is.
// The cluster's CONNECT handler does the sandbox lookup; the bridge
// stays sandbox-id-agnostic (per the device-can-hit-cluster ⇒ device-
// can-forward-to-any-sandbox-in-it auth model).
func (b *Broker) handleDeviceConnect(w http.ResponseWriter, r *http.Request, deviceID string, cluster *clusterConn) {
	target := r.RequestURI // CONNECT request-line is "host:port"
	if target == "" {
		target = r.URL.Host
	}
	if target == "" {
		http.Error(w, "CONNECT target required", http.StatusBadRequest)
		return
	}

	dial := func() (io.ReadWriteCloser, error) {
		out, err := cluster.sess.OpenStream()
		if err != nil {
			return nil, fmt.Errorf("open stream on cluster session: %w", err)
		}
		// Replay the CONNECT to the cluster.
		hdrs := http.Header{}
		hdrs.Set("X-Forwarded-Device", deviceID)
		if err := tunnel.WriteConnect(out, target, hdrs); err != nil {
			_ = out.Close()
			return nil, err
		}
		br := bufio.NewReader(out)
		if err := tunnel.ReadConnectResponse(br); err != nil {
			_ = out.Close()
			return nil, err
		}
		return tunnel.NewDrainedReadWriteCloser(out, br), nil
	}

	inbound, upstream, err := tunnel.AcceptConnect(w, r, dial)
	if err != nil {
		// AcceptConnect already wrote an HTTP error response.
		return
	}
	defer inbound.Close()
	defer upstream.Close()
	_ = tunnel.CopyStreams(r.Context(), inbound, upstream, tunnel.CopyOptions{})
}

// Snapshot returns a point-in-time view of who is connected. Useful
// for /healthz-plus endpoints and tests. Cheap; one RLock.
type Snapshot struct {
	Devices  []ConnInfo
	Clusters []ConnInfo
}

// ConnInfo is what Snapshot reports per connection.
type ConnInfo struct {
	ID           string
	ConnectedAgo time.Duration
}

// Snapshot returns the current connection census.
func (b *Broker) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	now := time.Now()
	s := Snapshot{
		Devices:  make([]ConnInfo, 0, len(b.devices)),
		Clusters: make([]ConnInfo, 0, len(b.clusters)),
	}
	// devices is multi-valued (one slice per CN); report each live
	// session so the snapshot reflects real concurrency.
	for _, list := range b.devices {
		for _, d := range list {
			s.Devices = append(s.Devices, ConnInfo{ID: d.id, ConnectedAgo: now.Sub(d.connected)})
		}
	}
	for _, c := range b.clusters {
		s.Clusters = append(s.Clusters, ConnInfo{ID: c.id, ConnectedAgo: now.Sub(c.connected)})
	}
	return s
}

// Shutdown closes every active session. Used by the cmd/bridge
// signal-handler so SIGTERM tears down cleanly instead of dangling
// goroutines.
func (b *Broker) Shutdown(_ context.Context) error {
	b.mu.Lock()
	for _, list := range b.devices {
		for _, d := range list {
			_ = d.sess.Close()
		}
	}
	for _, c := range b.clusters {
		_ = c.sess.Close()
	}
	b.devices = make(map[string][]*deviceConn)
	b.clusters = make(map[string]*clusterConn)
	b.mu.Unlock()
	return nil
}

// writeJSON is the bridge's tiny JSON-response helper. Kept inline so
// the bridge package depends on nothing beyond pkg/tunnel + stdlib.
func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}
