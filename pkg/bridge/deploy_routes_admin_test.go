package bridge

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Admin-gate tests for /api/deploy-routes — added after a security
// audit found mtlsGate alone is "any valid cert", which would let any
// signed-in device cert PUT the route table and wipe every other
// org's deploys. requireAdmin distinguishes admin SAN
// (urn:agentry:admin) from device-org SAN (urn:agentry:org:<x>).

// TestHandleDeployRoutes_RejectsNonAdminCert covers the bug fixed by
// the requireAdmin gate. A device cert with a regular org SAN is
// REQUIRED to fail — without the gate it would have wiped the table.
func TestHandleDeployRoutes_RejectsNonAdminCert(t *testing.T) {
	b := NewWithConfig(Config{}) // DevMode=false → enforce gate
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	// Pre-seed so we can prove a rejected PUT doesn't wipe anything.
	reg.Set(DeployRoute{Hostname: "untouched.agentry.live", ClusterID: "c"})

	cert := makeSignedCert(t, "device-alice", "urn:agentry:org:org_alpha")
	w := httptest.NewRecorder()
	r := reqWithPeer("PUT", "https://x.invalid/api/deploy-routes",
		cert)
	r.Body = http.NoBody
	b.handleDeployRoutesPut(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("device-org cert got %d on PUT; want 403", w.Code)
	}
	if _, ok := reg.Lookup("untouched.agentry.live"); !ok {
		t.Error("rejected PUT still wiped the table — gate is bypassed")
	}

	w2 := httptest.NewRecorder()
	r2 := reqWithPeer("GET", "https://x.invalid/api/deploy-routes", cert)
	b.handleDeployRoutesGet(w2, r2)
	if w2.Code != http.StatusForbidden {
		t.Errorf("device-org cert got %d on GET; want 403", w2.Code)
	}
}

// TestHandleDeployRoutes_AcceptsAdminCert is the positive counterpart:
// the syncer's admin cert (URI SAN urn:agentry:admin) keeps working.
func TestHandleDeployRoutes_AcceptsAdminCert(t *testing.T) {
	b := NewWithConfig(Config{})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)

	cert := makeSignedCert(t, "device-control-plane", "urn:agentry:admin")
	body := strings.NewReader(`{"routes":[{"hostname":"x.agentry.live","cluster_id":"c","org_id":"o"}]}`)
	w := httptest.NewRecorder()
	r := reqWithPeer("PUT", "https://x.invalid/api/deploy-routes", cert)
	r.Body = io.NopCloser(body)
	r.Header.Set("Content-Type", "application/json")
	b.handleDeployRoutesPut(w, r)
	if w.Code != http.StatusNoContent {
		t.Errorf("admin PUT got %d; want 204", w.Code)
	}
	if _, ok := reg.Lookup("x.agentry.live"); !ok {
		t.Error("admin PUT didn't install the route")
	}

	w2 := httptest.NewRecorder()
	r2 := reqWithPeer("GET", "https://x.invalid/api/deploy-routes", cert)
	b.handleDeployRoutesGet(w2, r2)
	if w2.Code != http.StatusOK {
		t.Errorf("admin GET got %d; want 200", w2.Code)
	}
}

// TestHandleDeployRoutes_RejectsNoPeerCert is what mtlsGate is
// SUPPOSED to catch upstream of requireAdmin. Belt-and-suspenders test
// — if mtlsGate ever lets a no-cert request through, requireAdmin
// must still 403 it.
func TestHandleDeployRoutes_RejectsNoPeerCert(t *testing.T) {
	b := NewWithConfig(Config{})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	reg.Set(DeployRoute{Hostname: "untouched.agentry.live", ClusterID: "c"})

	r := httptest.NewRequest("PUT", "https://x.invalid/api/deploy-routes", nil)
	r.Body = http.NoBody
	w := httptest.NewRecorder()
	b.handleDeployRoutesPut(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("no-cert PUT got %d; want 403", w.Code)
	}
	if _, ok := reg.Lookup("untouched.agentry.live"); !ok {
		t.Error("no-cert PUT still wiped the table")
	}
}

// TestHandleDeployRoutes_DevModeBypassesAdminGate exists so the local
// docker-compose dev stack keeps working without a real CA / cert
// chain. Mirrors handleClustersList's DevMode bypass.
func TestHandleDeployRoutes_DevModeBypassesAdminGate(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)

	body := strings.NewReader(`{"routes":[{"hostname":"x.agentry.live","cluster_id":"c"}]}`)
	r := httptest.NewRequest("PUT", "https://x.invalid/api/deploy-routes", body)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	b.handleDeployRoutesPut(w, r)
	if w.Code != http.StatusNoContent {
		t.Errorf("dev mode PUT got %d; want 204", w.Code)
	}
}

// Admin endpoints — GET /api/deploy-routes (debug census), PUT
// /api/deploy-routes (control-plane sync). The control-plane sync is
// fired every 10s by RunRouteResyncLoop. If PUT silently drops a
// route or accepts garbage, deploy URLs go dark.

func TestHandleDeployRoutesPut_ReplacesAll(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)

	// Pre-seed so we can verify ReplaceAll, not Append.
	reg.Set(DeployRoute{Hostname: "old.agentry.live", ClusterID: "c-old"})

	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	body := `{"routes":[
		{"hostname":"a.agentry.live","kind":"share","cluster_id":"c1","org_id":"o1","auth_mode":"public","sandbox_id":"s1","port":3000},
		{"hostname":"b.agentry.live","kind":"deployment","cluster_id":"c1","org_id":"o1","auth_mode":"org","deployment_id":"dep_1"}
	]}`
	req, _ := http.NewRequest("PUT", srv.URL+"/api/deploy-routes", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.StatusCode)
	}

	// Old route gone, two new routes present.
	if _, ok := reg.Lookup("old.agentry.live"); ok {
		t.Error("PUT did not clear pre-existing route 'old.agentry.live'")
	}
	if r, ok := reg.Lookup("a.agentry.live"); !ok {
		t.Error("PUT missed a.agentry.live")
	} else if r.Kind != "share" || r.SandboxID != "s1" || r.Port != 3000 {
		t.Errorf("PUT garbled a.agentry.live: %+v", r)
	}
	if r, ok := reg.Lookup("b.agentry.live"); !ok {
		t.Error("PUT missed b.agentry.live")
	} else if r.Kind != "deployment" || r.DeploymentID != "dep_1" {
		t.Errorf("PUT garbled b.agentry.live: %+v", r)
	}
}

func TestHandleDeployRoutesPut_EmptyRoutesClears(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	reg.Set(DeployRoute{Hostname: "x.agentry.live", ClusterID: "c"})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("PUT", srv.URL+"/api/deploy-routes",
		strings.NewReader(`{"routes":[]}`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if all := reg.All(); len(all) != 0 {
		t.Errorf("after empty PUT, registry has %d routes; want 0", len(all))
	}
}

func TestHandleDeployRoutesPut_BadJSON(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	// Pre-seed so we can prove a bad PUT did NOT wipe the table.
	reg.Set(DeployRoute{Hostname: "keep.agentry.live", ClusterID: "c"})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("PUT", srv.URL+"/api/deploy-routes",
		strings.NewReader(`not json at all {`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", resp.StatusCode)
	}
	if _, ok := reg.Lookup("keep.agentry.live"); !ok {
		t.Error("bad PUT wiped pre-existing routes — atomicity broken")
	}
}

func TestHandleDeployRoutesPut_NotConfigured(t *testing.T) {
	// Broker has no AttachDeploy. PUT must 503, not panic, and must
	// not silently accept the routes (because there's nowhere to put
	// them).
	b := NewWithConfig(Config{DevMode: true})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("PUT", srv.URL+"/api/deploy-routes",
		strings.NewReader(`{"routes":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", resp.StatusCode)
	}
}

func TestHandleDeployRoutesGet_ReturnsAll(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	reg.Set(DeployRoute{Hostname: "a.agentry.live", Kind: "share", ClusterID: "c1"})
	reg.Set(DeployRoute{Hostname: "b.agentry.live", Kind: "deployment", ClusterID: "c1"})

	resp, err := http.Get(srv.URL + "/api/deploy-routes")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var got deployRoutesEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 2 {
		t.Errorf("got %d routes; want 2", len(got.Routes))
	}
}

func TestHandleDeployRoutesGet_NotConfigured(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/deploy-routes")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", resp.StatusCode)
	}
}

// Round-trip: a PUT followed by a GET must echo exactly what we put.
// This is the contract the control plane's RunRouteResyncLoop relies
// on — we want to confirm our resync loop's view is authoritative on
// the bridge.
func TestHandleDeployRoutes_PutGetRoundTrip(t *testing.T) {
	b := NewWithConfig(Config{DevMode: true})
	reg := NewDeployRegistry()
	b.AttachDeploy(reg)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	in := []DeployRoute{
		{Hostname: "alpha.agentry.live", Kind: "share", ClusterID: "c1",
			OrgID: "o1", AuthMode: "public", SandboxID: "s1", Port: 3000},
		{Hostname: "beta.agentry.live", Kind: "deployment", ClusterID: "c1",
			OrgID: "o1", AuthMode: "org", DeploymentID: "d1"},
	}
	body, _ := json.Marshal(map[string]any{"routes": in})
	putReq, _ := http.NewRequest("PUT", srv.URL+"/api/deploy-routes", bytes.NewReader(body))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	putResp.Body.Close()

	getResp, err := http.Get(srv.URL + "/api/deploy-routes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()
	var got deployRoutesEnvelope
	_ = json.NewDecoder(getResp.Body).Decode(&got)
	if len(got.Routes) != len(in) {
		t.Fatalf("round trip lost rows: in=%d out=%d", len(in), len(got.Routes))
	}
	// Index by hostname so order doesn't matter (maps are unordered).
	byHost := map[string]DeployRoute{}
	for _, r := range got.Routes {
		byHost[r.Hostname] = r
	}
	for _, want := range in {
		got, ok := byHost[want.Hostname]
		if !ok {
			t.Errorf("round trip dropped %s", want.Hostname)
			continue
		}
		if got != want {
			t.Errorf("round trip mutated row:\n in = %+v\n out= %+v", want, got)
		}
	}
}
