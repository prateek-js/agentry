package provisioner

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/agentry-ai/agentry/pkg/auth"
)

// fakeRuntime stands in for a sandbox runtime: it records the
// X-Sandbox-API-Key header it was reached with.
func fakeRuntimeSrv(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(auth.HeaderName)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotKey
}

// proxyProvisioner wires a Provisioner whose backend resolves sandbox
// "sb1" to the fake runtime, with the given runtime key.
func proxyProvisioner(t *testing.T, runtimeKey string) (*Provisioner, *string) {
	t.Helper()
	srv, gotKey := fakeRuntimeSrv(t)
	u, _ := url.Parse(srv.URL)
	host, portStr, _ := strings.Cut(u.Host, ":")
	port, _ := strconv.Atoi(portStr)

	mock := NewMockBackend()
	mock.preSeed("sb1", host, int32(port))
	cfg := Config{Namespace: "test-ns", NodeHost: host, RuntimeAPIKey: runtimeKey}
	return NewWithKey(cfg, mock, ""), gotKey
}

func TestRuntimeProxy_StampsAPIKey(t *testing.T) {
	p, gotKey := proxyProvisioner(t, "rkey")
	req := httptest.NewRequest(http.MethodGet, "/api/sandboxes/sb1/runtime/v1/sandbox", nil)
	rec := httptest.NewRecorder()
	p.handleRuntimeProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d", rec.Code)
	}
	if *gotKey != "rkey" {
		t.Fatalf("runtime saw key %q; want stamped %q", *gotKey, "rkey")
	}
}

func TestRuntimeProxy_OverwritesClientSuppliedKey(t *testing.T) {
	p, gotKey := proxyProvisioner(t, "rkey")
	req := httptest.NewRequest(http.MethodGet, "/api/sandboxes/sb1/runtime/v1/sandbox", nil)
	// A caller trying to smuggle its own key must not have it forwarded.
	req.Header.Set(auth.HeaderName, "attacker-controlled")
	rec := httptest.NewRecorder()
	p.handleRuntimeProxy(rec, req)

	if *gotKey != "rkey" {
		t.Fatalf("runtime saw %q; client-supplied key should be overwritten with %q", *gotKey, "rkey")
	}
}

func TestRuntimeProxy_NoKeyLeavesHeaderEmpty(t *testing.T) {
	// Local-dev posture: no runtime key → nothing stamped.
	p, gotKey := proxyProvisioner(t, "")
	req := httptest.NewRequest(http.MethodGet, "/api/sandboxes/sb1/runtime/v1/sandbox", nil)
	rec := httptest.NewRecorder()
	p.handleRuntimeProxy(rec, req)

	if *gotKey != "" {
		t.Fatalf("runtime saw key %q; want none when disabled", *gotKey)
	}
}

func TestCreate_PropagatesRuntimeKeyToSpec(t *testing.T) {
	mock := NewMockBackend()
	cfg := Config{
		Namespace: "test-ns", SandboxImage: "img:latest", NodeHost: "test-host",
		Labels: map[string]string{"app": "ad-sandbox"}, RuntimeAPIKey: "rkey",
	}
	p := NewWithKey(cfg, mock, "") // inbound auth off for the test
	p.SetReadyProbe(nil)
	ts := httptest.NewServer(p.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/sandboxes", "application/json",
		strings.NewReader(`{"sandbox_id":"s1","thread_id":"t1"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	spec, ok := mock.Spec("sandbox-s1")
	if !ok {
		t.Fatal("sandbox not created")
	}
	if spec.RuntimeAPIKey != "rkey" {
		t.Fatalf("spec.RuntimeAPIKey = %q; want the configured key so the backend injects it", spec.RuntimeAPIKey)
	}
}
