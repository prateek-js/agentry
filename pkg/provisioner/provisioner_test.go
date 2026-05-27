package provisioner

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentry/agentry/pkg/auth"
)

func newTestProvisioner(t *testing.T, key string) (*httptest.Server, *MockBackend) {
	t.Helper()
	mock := NewMockBackend()
	cfg := Config{
		Namespace:    "test-ns",
		SandboxImage: "test-image:latest",
		NodeHost:     "test-host",
		ListenAddr:   ":0",
		Labels:       map[string]string{"app": "ad-sandbox"},
	}
	p := NewWithKey(cfg, mock, key)
	// Mock backend doesn't actually run a runtime, so skip the
	// post-create /health probe. The real provisioner keeps it.
	p.SetReadyProbe(nil)
	ts := httptest.NewServer(p.Handler())
	t.Cleanup(ts.Close)
	return ts, mock
}

func TestProvisionerHealthExemptFromAuth(t *testing.T) {
	ts, _ := newTestProvisioner(t, "secret")

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d; want 200 (exempt)", resp.StatusCode)
	}
}

func TestProvisionerCreateRequiresAuth(t *testing.T) {
	ts, mock := newTestProvisioner(t, "secret")

	body := strings.NewReader(`{"sandbox_id":"s1","thread_id":"t1"}`)
	resp, err := http.Post(ts.URL+"/api/sandboxes", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /api/sandboxes = %d; want 401", resp.StatusCode)
	}
	if mock.PodCount() != 0 {
		t.Fatalf("auth bypass: %d pods created", mock.PodCount())
	}
}

func TestProvisionerCreateWithValidKey(t *testing.T) {
	ts, mock := newTestProvisioner(t, "secret")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sandboxes",
		bytes.NewBufferString(`{"sandbox_id":"s1","thread_id":"t1"}`))
	req.Header.Set(auth.HeaderName, "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated create = %d; want 200", resp.StatusCode)
	}
	if mock.PodCount() != 1 {
		t.Fatalf("want 1 pod created, got %d", mock.PodCount())
	}
}

func TestProvisionerListRequiresAuth(t *testing.T) {
	ts, _ := newTestProvisioner(t, "secret")

	resp, err := http.Get(ts.URL + "/api/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/sandboxes = %d; want 401", resp.StatusCode)
	}
}

func TestProvisionerAuthDisabledByEmptyKey(t *testing.T) {
	ts, _ := newTestProvisioner(t, "")

	resp, err := http.Get(ts.URL + "/api/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with auth disabled, GET /api/sandboxes = %d; want 200", resp.StatusCode)
	}
}
