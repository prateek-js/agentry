package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentry/agentry/pkg/auth"
)

// newTestServer builds a runtime.Server wired with the given key and returns
// an httptest.Server driving its full middleware chain.
func newTestServer(t *testing.T, key string) *httptest.Server {
	t.Helper()
	s := NewWithKey(":0", key)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestServerHealthExemptFromAuth(t *testing.T) {
	ts := newTestServer(t, "secret")

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d; want 200 (must be exempt)", resp.StatusCode)
	}
}

func TestServerSandboxEndpointRequiresKey(t *testing.T) {
	ts := newTestServer(t, "secret")

	// No header → 401.
	resp, err := http.Get(ts.URL + "/v1/sandbox")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/sandbox = %d; want 401", resp.StatusCode)
	}

	// Correct key → 200.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/sandbox", nil)
	req.Header.Set(auth.HeaderName, "secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /v1/sandbox = %d; want 200", resp.StatusCode)
	}
}

func TestServerCORSPreflightAlwaysAllowed(t *testing.T) {
	ts := newTestServer(t, "secret")

	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/v1/sandbox", nil)
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Either the CORS middleware short-circuits with 204, or the auth-OPTIONS
	// bypass forwards to a handler that returns OK. Both are acceptable; what
	// must not happen is 401.
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("CORS preflight blocked by auth: status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got == "" {
		t.Errorf("preflight missing CORS headers: %v", resp.Header)
	}
}

func TestServerAuthDisabledWhenKeyEmpty(t *testing.T) {
	ts := newTestServer(t, "")

	resp, err := http.Get(ts.URL + "/v1/sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unauthenticated /v1/sandbox (key disabled) = %d; want 200", resp.StatusCode)
	}
}
