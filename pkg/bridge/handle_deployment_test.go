package bridge

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentry-ai/agentry/pkg/tunnel"
)

// HandleDeployment tests. Production path: <host>.agentry.live arrives
// at the bridge, we look up the route, dispatch through the cluster
// tunnel, rewrite the URL to the cluster's runtime/deployment API
// shape, and proxy. Every failure shape on that path is a missed
// deploy URL in production.

// startBrokerWithRegistry pairs a Broker with an attached
// DeployRegistry behind an httptest.Server, both DevMode for the
// non-org-isolation tests (matches the pattern bridge_test.go uses
// for the cluster-routing tests).
func startBrokerWithRegistry(t *testing.T) (*Broker, *DeployRegistry, *httptest.Server) {
	t.Helper()
	b := NewWithConfig(Config{DevMode: true})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	srv := httptest.NewServer(http.HandlerFunc(b.HandleDeployment))
	t.Cleanup(srv.Close)
	return b, reg, srv
}

func TestHandleDeployment_RegistryNotConfigured(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	srv := httptest.NewServer(http.HandlerFunc(b.HandleDeployment))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", resp.StatusCode)
	}
}

func TestHandleDeployment_UnknownHostname(t *testing.T) {
	_, _, srv := startBrokerWithRegistry(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (registry empty → unknown)", resp.StatusCode)
	}
}

func TestHandleDeployment_ClusterOffline(t *testing.T) {
	_, reg, srv := startBrokerWithRegistry(t)

	// Route exists but no cluster is connected for it.
	reg.Set(DeployRoute{
		Hostname:  "x.agentry.live",
		Kind:      "share",
		ClusterID: "ghost",
		SandboxID: "sb_x",
		Port:      3000,
		OrgID:     "org_alpha",
	})
	// httptest.NewServer URL is http://127.0.0.1:XXXX. We have to
	// override r.Host so the bridge looks up "x.agentry.live", not
	// "127.0.0.1:XXXX". Use a raw client + manual Host header.
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "x.agentry.live"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "offline") {
		t.Errorf("body = %q; want substring 'offline'", body)
	}
}

func TestHandleDeployment_ShareRouteRewritesPath(t *testing.T) {
	b, reg, srv := startBrokerWithRegistry(t)

	// Stand up a "cluster" that records the upstream path the bridge
	// rewrote into. The Director must produce
	//   /api/sandboxes/<sid>/runtime/v1/proxy/<port><rest>
	// — anything else and the cluster-side proxy lookup misses.
	bsrv := httptest.NewServer(b.Handler())
	t.Cleanup(bsrv.Close)
	gotPath := make(chan string, 1)
	joinCluster(t, bsrv.URL, "homelab", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotPath <- r.URL.Path:
		default:
		}
		w.WriteHeader(204)
	}))
	mustHaveClusters(t, bsrv.URL, []string{"homelab"})

	reg.Set(DeployRoute{
		Hostname:  "live-7c2.agentry.live",
		Kind:      "share",
		ClusterID: "homelab",
		SandboxID: "sb_abc",
		Port:      3000,
		OrgID:     "org_alpha",
	})

	req, _ := http.NewRequest("GET", srv.URL+"/dashboard/index.html", nil)
	req.Host = "live-7c2.agentry.live"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	select {
	case p := <-gotPath:
		want := "/api/sandboxes/sb_abc/runtime/v1/proxy/3000/dashboard/index.html"
		if p != want {
			t.Errorf("upstream path = %q; want %q", p, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cluster never received the proxied request")
	}
}

func TestHandleDeployment_DeploymentRouteRewritesPath(t *testing.T) {
	b, reg, srv := startBrokerWithRegistry(t)
	bsrv := httptest.NewServer(b.Handler())
	t.Cleanup(bsrv.Close)
	gotPath := make(chan string, 1)
	joinCluster(t, bsrv.URL, "homelab", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotPath <- r.URL.Path:
		default:
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("hello"))
	}))
	mustHaveClusters(t, bsrv.URL, []string{"homelab"})

	reg.Set(DeployRoute{
		Hostname:     "shopwave-ecom-8c62.agentry.live",
		Kind:         "deployment",
		ClusterID:    "homelab",
		DeploymentID: "dep_abc",
		OrgID:        "org_alpha",
	})

	req, _ := http.NewRequest("GET", srv.URL+"/products?cat=shoes", nil)
	req.Host = "shopwave-ecom-8c62.agentry.live"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}

	select {
	case p := <-gotPath:
		want := "/api/deployments/dep_abc/proxy/products"
		if p != want {
			t.Errorf("upstream path = %q; want %q", p, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cluster never received the proxied request")
	}
}

func TestHandleDeployment_PreservesQueryString(t *testing.T) {
	b, reg, srv := startBrokerWithRegistry(t)
	bsrv := httptest.NewServer(b.Handler())
	t.Cleanup(bsrv.Close)
	gotQuery := make(chan string, 1)
	joinCluster(t, bsrv.URL, "h", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotQuery <- r.URL.RawQuery:
		default:
		}
		w.WriteHeader(204)
	}))
	mustHaveClusters(t, bsrv.URL, []string{"h"})

	reg.Set(DeployRoute{
		Hostname: "q.agentry.live", Kind: "deployment",
		ClusterID: "h", DeploymentID: "d", OrgID: "org_x",
	})
	req, _ := http.NewRequest("GET", srv.URL+"/p?cat=shoes&size=10", nil)
	req.Host = "q.agentry.live"
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	select {
	case q := <-gotQuery:
		if q != "cat=shoes&size=10" {
			t.Errorf("query = %q; want cat=shoes&size=10", q)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never got the request")
	}
}

// TestHandleDeployment_StripsClerkOnly verifies the bridge's
// selective cookie strip: Clerk dashboard cookies (and Authorization)
// are dropped before they reach the user's app, but every other
// cookie passes through unchanged. Pre-m2 the bridge stripped the
// entire Cookie header, which broke the in-sandbox authproxy sidecar
// because agentry_csrf and agentry_session never reached it.
func TestHandleDeployment_StripsClerkOnly(t *testing.T) {
	cases := []struct {
		name     string
		cookie   string
		wantUp   string // expected Cookie header upstream
		wantAuth string // expected Authorization header upstream (always "")
	}{
		{
			name:   "only Clerk cookies — strip entirely",
			cookie: "__session=clerk; __client_uat=ts; __client_state=foo",
			wantUp: "",
		},
		{
			name:   "only authproxy cookies — pass through",
			cookie: "agentry_csrf=tokA; agentry_session=tokB",
			wantUp: "agentry_csrf=tokA; agentry_session=tokB",
		},
		{
			name:   "mixed — strip Clerk, keep authproxy",
			cookie: "__session=clerk; agentry_csrf=tokA; __client_uat=ts; agentry_session=tokB",
			wantUp: "agentry_csrf=tokA; agentry_session=tokB",
		},
		{
			name:   "third-party cookie (unknown name) — pass through",
			cookie: "ga_session=ga; agentry_csrf=tokA",
			wantUp: "ga_session=ga; agentry_csrf=tokA",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			b, reg, srv := startBrokerWithRegistry(t)
			bsrv := httptest.NewServer(b.Handler())
			t.Cleanup(bsrv.Close)

			got := make(chan http.Header, 1)
			joinCluster(t, bsrv.URL, "h", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hdrCopy := r.Header.Clone()
				select {
				case got <- hdrCopy:
				default:
				}
				w.WriteHeader(204)
			}))
			mustHaveClusters(t, bsrv.URL, []string{"h"})

			reg.Set(DeployRoute{
				Hostname: "x.agentry.live", Kind: "deployment",
				ClusterID: "h", DeploymentID: "d", OrgID: "org_x",
			})
			req, _ := http.NewRequest("GET", srv.URL+"/", nil)
			req.Host = "x.agentry.live"
			req.Header.Set("Cookie", tc.cookie)
			req.Header.Set("Authorization", "Bearer pat_secret")
			req.Header.Set("X-Stay", "keep-me")
			resp, _ := http.DefaultClient.Do(req)
			resp.Body.Close()

			select {
			case h := <-got:
				if got := h.Get("Cookie"); got != tc.wantUp {
					t.Errorf("Cookie upstream: got %q, want %q", got, tc.wantUp)
				}
				if h.Get("Authorization") != "" {
					t.Errorf("Authorization leaked: %q", h.Get("Authorization"))
				}
				if h.Get("X-Stay") != "keep-me" {
					t.Errorf("X-Stay dropped: %q", h.Get("X-Stay"))
				}
			case <-time.After(2 * time.Second):
				t.Fatal("upstream never got the request")
			}
		})
	}
}

func TestHandleDeployment_StripsPortFromHostHeader(t *testing.T) {
	// In production the bridge sits behind autocert TLS on :443 so the
	// Host header is "x.agentry.live"; but if someone curls with
	// "x.agentry.live:443" we still need to find the route.
	_, reg, srv := startBrokerWithRegistry(t)
	reg.Set(DeployRoute{
		Hostname:  "x.agentry.live",
		Kind:      "share",
		ClusterID: "ghost", // doesn't matter, we just want to confirm Lookup hit
		SandboxID: "s",
		Port:      80,
		OrgID:     "org_x",
	})
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "x.agentry.live:8443"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	// Cluster isn't connected → 502 (route was FOUND), not 404 (route
	// missing). 404 here would mean port-stripping regressed.
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (route found, cluster offline)", resp.StatusCode)
	}
}

func TestHandleDeployment_ShareMissingFields(t *testing.T) {
	b, reg, srv := startBrokerWithRegistry(t)
	bsrv := httptest.NewServer(b.Handler())
	t.Cleanup(bsrv.Close)
	joinCluster(t, bsrv.URL, "h", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))
	mustHaveClusters(t, bsrv.URL, []string{"h"})

	cases := []struct {
		name string
		r    DeployRoute
	}{
		{"missing sandbox_id", DeployRoute{
			Hostname: "x.agentry.live", Kind: "share",
			ClusterID: "h", Port: 3000, OrgID: "o",
		}},
		{"missing port", DeployRoute{
			Hostname: "x.agentry.live", Kind: "share",
			ClusterID: "h", SandboxID: "s", OrgID: "o",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg.ReplaceAll([]DeployRoute{tc.r})
			req, _ := http.NewRequest("GET", srv.URL+"/", nil)
			req.Host = "x.agentry.live"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("status = %d; want 500 for malformed route", resp.StatusCode)
			}
		})
	}
}

func TestHandleDeployment_DeploymentMissingID(t *testing.T) {
	b, reg, srv := startBrokerWithRegistry(t)
	bsrv := httptest.NewServer(b.Handler())
	t.Cleanup(bsrv.Close)
	joinCluster(t, bsrv.URL, "h", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	}))
	mustHaveClusters(t, bsrv.URL, []string{"h"})

	reg.Set(DeployRoute{
		Hostname: "x.agentry.live", Kind: "deployment",
		ClusterID: "h", OrgID: "o", // DeploymentID empty
	})
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "x.agentry.live"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", resp.StatusCode)
	}
}

func TestHandleDeployment_LegacyEmptyKindDefaultsToShare(t *testing.T) {
	// Old in-memory routes (pre-Kind field) MUST still route as shares.
	// Otherwise a bridge restart partway through a rolling deploy of
	// the Kind change would 500 every share URL.
	b, reg, srv := startBrokerWithRegistry(t)
	bsrv := httptest.NewServer(b.Handler())
	t.Cleanup(bsrv.Close)
	gotPath := make(chan string, 1)
	joinCluster(t, bsrv.URL, "h", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotPath <- r.URL.Path:
		default:
		}
		w.WriteHeader(204)
	}))
	mustHaveClusters(t, bsrv.URL, []string{"h"})

	reg.Set(DeployRoute{
		Hostname:  "x.agentry.live",
		Kind:      "", // legacy
		ClusterID: "h",
		SandboxID: "sb1",
		Port:      4000,
		OrgID:     "org_x",
	})
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "x.agentry.live"
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	select {
	case p := <-gotPath:
		if !strings.HasPrefix(p, "/api/sandboxes/sb1/runtime/v1/proxy/4000") {
			t.Errorf("legacy empty Kind did not default to share path: got %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received request")
	}
}

// startProdBrokerWithRegistry stands up a non-DevMode broker so the
// org-isolation check at HandleDeployment fires. Production handshake
// requires a peer cert presenting an org URI SAN, so we register a
// cluster session by hand instead of going through tunnel.Dial.
func startProdBrokerWithRegistry(t *testing.T) (*Broker, *DeployRegistry, *httptest.Server) {
	t.Helper()
	b := NewWithConfig(Config{}) // DevMode=false → enforce org check
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	srv := httptest.NewServer(http.HandlerFunc(b.HandleDeployment))
	t.Cleanup(srv.Close)
	return b, reg, srv
}

// seedCluster wires a clusterConn into the broker map without going
// through the real tunnel handshake. The RoundTripper holds a nil
// session so any actual RoundTrip call returns ErrSessionClosed
// cleanly (the bridge's ErrorHandler converts that to 502) rather
// than panicking on a nil receiver. These tests assert on which gate
// fired, not on a successful proxy.
func seedCluster(b *Broker, id, orgID string) {
	b.mu.Lock()
	b.clusters[id] = &clusterConn{
		id:        id,
		orgID:     orgID,
		connected: time.Now(),
		rt:        tunnel.NewRoundTripper(nil),
	}
	b.mu.Unlock()
}

// TestHandleDeployment_BlocksCrossOrgRoute is the defense-in-depth
// check added in the same change as this test. A route whose OrgID
// disagrees with the cluster's cert-stamped org must be refused — even
// if the control plane pushed it. Returns 502 (same shape as "cluster
// offline") so no information leaks about whether the cluster name
// exists somewhere.
func TestHandleDeployment_BlocksCrossOrgRoute(t *testing.T) {
	b, reg, srv := startProdBrokerWithRegistry(t)
	seedCluster(b, "homelab", "org_beta") // cluster belongs to beta

	// Route claims org_alpha but points at beta's cluster — exactly
	// the corrupted-route shape the check is for.
	reg.Set(DeployRoute{
		Hostname:     "x.agentry.live",
		Kind:         "deployment",
		ClusterID:    "homelab",
		DeploymentID: "dep_1",
		OrgID:        "org_alpha",
		AuthMode:     "public",
	})
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "x.agentry.live"
	// Some HTTP layer above us would have its own TLS state; we don't
	// need that to hit HandleDeployment.
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{}}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 for cross-org route", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "inconsistent") {
		t.Errorf("body = %q; want substring 'inconsistent'", body)
	}
}

func TestHandleDeployment_AllowsMatchingOrgRoute(t *testing.T) {
	// Counterpart to the cross-org test: the SAME route, but with the
	// org_id corrected, MUST clear the org-isolation check. We seed a
	// cluster whose RoundTripper is nil-session — any RoundTrip returns
	// ErrSessionClosed, which the bridge's ErrorHandler converts to a
	// distinctive 502 message. The discriminator is the body: the new
	// org check writes "inconsistent"; the session-closed path writes
	// "cluster session closed mid-request". If the latter shows up we
	// know we reached the proxy → cleared the org check.
	b := NewWithConfig(Config{})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	seedCluster(b, "homelab", "org_alpha")
	srv := httptest.NewServer(http.HandlerFunc(b.HandleDeployment))
	t.Cleanup(srv.Close)

	reg.Set(DeployRoute{
		Hostname:     "x.agentry.live",
		Kind:         "deployment",
		ClusterID:    "homelab",
		DeploymentID: "dep_1",
		OrgID:        "org_alpha", // matches cluster
		AuthMode:     "public",
	})
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "x.agentry.live"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "inconsistent") {
		t.Errorf("matching-org route was rejected by org check: %s", body)
	}
	if !strings.Contains(string(body), "session closed") {
		t.Errorf("expected to hit upstream proxy (session-closed body); got %s", body)
	}
}
