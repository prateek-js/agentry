package bridge

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// Tests for the org-SAN tenancy enforcement added to handleClustersList
// and proxyToCluster. These don't spin up TLS — they construct
// requests with a synthetic ConnectionState carrying the URI SANs the
// bridge looks for, so we exercise the production code path without
// the per-test cert chain noise.

// makeSignedCert mints a tiny self-signed cert and returns the leaf.
// We don't need a real CA chain; the bridge code only ever reads
// PeerCertificates[0].URIs and PeerCertificates[0].Subject.CommonName.
func makeSignedCert(t *testing.T, cn string, uris ...string) *x509.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	parsedURIs := make([]*url.URL, 0, len(uris))
	for _, u := range uris {
		p, err := url.Parse(u)
		if err != nil {
			t.Fatalf("bad URI %q: %v", u, err)
		}
		parsedURIs = append(parsedURIs, p)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		URIs:         parsedURIs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// reqWithPeer builds a request that looks like it came over a verified
// TLS connection presenting cert. Bridge code only inspects r.TLS, so
// the rest of the request can be whatever.
func reqWithPeer(method, urlStr string, cert *x509.Certificate) *http.Request {
	r := httptest.NewRequest(method, urlStr, nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	return r
}

func TestPeerOrgIdentity_OrgSAN(t *testing.T) {
	cert := makeSignedCert(t, "device-alice", "urn:agentry:org:org_alpha")
	r := reqWithPeer("GET", "https://x.invalid/", cert)
	org, admin := peerOrgIdentity(r)
	if org != "org_alpha" || admin {
		t.Errorf("got org=%q admin=%v, want org=org_alpha admin=false", org, admin)
	}
}

func TestPeerOrgIdentity_AdminSAN(t *testing.T) {
	cert := makeSignedCert(t, "device-control-plane", "urn:agentry:admin")
	r := reqWithPeer("GET", "https://x.invalid/", cert)
	org, admin := peerOrgIdentity(r)
	if org != "" || !admin {
		t.Errorf("got org=%q admin=%v, want org=\"\" admin=true", org, admin)
	}
}

func TestPeerOrgIdentity_NoSANs(t *testing.T) {
	cert := makeSignedCert(t, "device-legacy") // no URIs at all
	r := reqWithPeer("GET", "https://x.invalid/", cert)
	org, admin := peerOrgIdentity(r)
	if org != "" || admin {
		t.Errorf("got org=%q admin=%v, want empty/false for legacy cert", org, admin)
	}
}

func TestPeerOrgIdentity_NoTLSState(t *testing.T) {
	r := httptest.NewRequest("GET", "https://x.invalid/", nil)
	org, admin := peerOrgIdentity(r)
	if org != "" || admin {
		t.Errorf("got org=%q admin=%v, want empty/false when r.TLS is nil", org, admin)
	}
}

func TestHandleClustersList_FiltersByOrg(t *testing.T) {
	b := NewWithConfig(Config{}) // DevMode=false → enforce
	// Seed two clusters in different orgs by hand. We don't need a
	// real session here; handleClustersList only reads the map.
	b.clusters["alpha-prod"] = &clusterConn{id: "alpha-prod", orgID: "org_alpha", connected: time.Now()}
	b.clusters["beta-prod"] = &clusterConn{id: "beta-prod", orgID: "org_beta", connected: time.Now()}

	cert := makeSignedCert(t, "device-alice", "urn:agentry:org:org_alpha")
	r := reqWithPeer("GET", "https://x.invalid/api/clusters", cert)
	w := httptest.NewRecorder()
	b.handleClustersList(w, r)

	var resp struct {
		Clusters []struct {
			ID string `json:"id"`
		} `json:"clusters"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Clusters) != 1 || resp.Clusters[0].ID != "alpha-prod" {
		t.Errorf("alpha caller saw %+v, want just alpha-prod", resp.Clusters)
	}
}

func TestHandleClustersList_AdminSeesAll(t *testing.T) {
	b := NewWithConfig(Config{})
	b.clusters["alpha-prod"] = &clusterConn{id: "alpha-prod", orgID: "org_alpha", connected: time.Now()}
	b.clusters["beta-prod"] = &clusterConn{id: "beta-prod", orgID: "org_beta", connected: time.Now()}

	cert := makeSignedCert(t, "device-control-plane", "urn:agentry:admin")
	r := reqWithPeer("GET", "https://x.invalid/api/clusters", cert)
	w := httptest.NewRecorder()
	b.handleClustersList(w, r)

	var resp struct {
		Clusters []struct {
			ID string `json:"id"`
		} `json:"clusters"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Clusters) != 2 {
		t.Errorf("admin saw %d clusters, want 2", len(resp.Clusters))
	}
}

func TestHandleClustersList_DevModeBypass(t *testing.T) {
	// DevMode=true means no TLS state at all on inbound requests.
	// The handler must still answer (with the full census), so the
	// existing localhost-stack tests keep working.
	b := NewWithConfig(Config{DevMode: true})
	b.clusters["a"] = &clusterConn{id: "a", connected: time.Now()}
	b.clusters["b"] = &clusterConn{id: "b", connected: time.Now()}

	r := httptest.NewRequest("GET", "https://x.invalid/api/clusters", nil)
	w := httptest.NewRecorder()
	b.handleClustersList(w, r)
	var resp struct {
		Clusters []struct {
			ID string `json:"id"`
		} `json:"clusters"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Clusters) != 2 {
		t.Errorf("dev mode saw %d, want 2", len(resp.Clusters))
	}
}

func TestProxyToCluster_BlocksCrossOrg(t *testing.T) {
	// alice (org_alpha) asks the bridge to route to beta-prod, which
	// belongs to org_beta. Without the SAN filter this would happily
	// proxy. We assert we get the same response as for a totally
	// missing cluster: 502, no leak that the name exists somewhere.
	b := NewWithConfig(Config{})
	b.clusters["beta-prod"] = &clusterConn{id: "beta-prod", orgID: "org_beta", connected: time.Now()}

	cert := makeSignedCert(t, "device-alice", "urn:agentry:org:org_alpha")
	r := reqWithPeer("GET", "https://x.invalid/api/clusters/beta-prod/sandboxes", cert)
	w := httptest.NewRecorder()
	b.proxyToCluster(w, r, "beta-prod", "/api/sandboxes")
	if w.Code != http.StatusBadGateway {
		t.Errorf("cross-org route returned %d, want %d", w.Code, http.StatusBadGateway)
	}
}

// TestPerSandboxControlPlaneRoutes_Registered pins the per-sandbox
// control-plane routes (single GET, POST /renew, GET/POST /secrets)
// into the bridge mux. The handlers themselves are one-line delegations
// to proxyToCluster, which is already covered by
// TestProxyToCluster_BlocksCrossOrg; this test catches the failure mode
// where a route gets accidentally deleted from Handler()'s mux and
// silently 404s.
//
// We assert "502 because the cluster isn't joined" rather than
// "404 because the route is gone" — proxyToCluster returns the former
// when the named cluster isn't currently connected.
func TestPerSandboxControlPlaneRoutes_Registered(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"single sandbox GET", "GET", "/api/clusters/anywhere/sandboxes/sb1"},
		{"renew POST", "POST", "/api/clusters/anywhere/sandboxes/sb1/renew"},
		{"secrets list GET", "GET", "/api/clusters/anywhere/sandboxes/sb1/secrets"},
		{"secret set POST", "POST", "/api/clusters/anywhere/sandboxes/sb1/secrets"},
		{"deploy-push POST", "POST", "/api/clusters/anywhere/sandboxes/sb1/deploy-push"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, srv.URL+tc.path, http.NoBody)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			// 502 = route exists, proxyToCluster ran, no such cluster.
			// 404 = route never registered (regression).
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("%s status = %d; want 502 (route registered but cluster offline)",
					tc.method, resp.StatusCode)
			}
		})
	}
}

// keep the yamux import — bridge_test.go uses it too but goimports
// won't trim it from this file unless we touch it.
var _ = yamux.DefaultConfig
