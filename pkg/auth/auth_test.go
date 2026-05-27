package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestDisabledWhenKeyEmpty(t *testing.T) {
	a := New("")
	if a.Enabled() {
		t.Fatalf("Enabled() = true; want false for empty key")
	}
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 (auth disabled)", resp.StatusCode)
	}
}

func TestValidKeyHeaderAllows(t *testing.T) {
	const key = "s3cr3t-key"
	a := New(key)
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/foo", nil)
	req.Header.Set(HeaderName, key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
}

func TestValidBearerAuthorizationAllows(t *testing.T) {
	const key = "s3cr3t-key"
	a := New(key)
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/foo", nil)
	req.Header.Set(AuthorizationHeader, "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
}

func TestMissingHeaderRejected(t *testing.T) {
	a := New("s3cr3t-key")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/foo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q; want Bearer challenge", got)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	a := New("right-key")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/foo", nil)
	req.Header.Set(HeaderName, "wrong-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
}

func TestWrongKeyDifferentLengthRejected(t *testing.T) {
	// subtle.ConstantTimeCompare returns 0 on length mismatch without leaking
	// timing — verify behaviorally that mismatched-length keys are rejected.
	a := New("right-key-32-chars-aaaaaaaaaaaaa")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/foo", nil)
	req.Header.Set(HeaderName, "short")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
}

func TestEmptyKeyHeaderRejected(t *testing.T) {
	a := New("right-key")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/foo", nil)
	req.Header.Set(HeaderName, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty header should not bypass auth, got %d", resp.StatusCode)
	}
}

func TestExemptPathBypassesAuth(t *testing.T) {
	a := New("s3cr3t-key", "/health", "/ready")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	for _, p := range []string{"/health", "/ready"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("path %s: status = %d; want 200 (exempt)", p, resp.StatusCode)
		}
	}
}

func TestExemptPathDoesNotMatchPrefix(t *testing.T) {
	// /healthy must NOT be exempt just because /health is.
	a := New("s3cr3t-key", "/health")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthy")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/healthy should require auth (only exact /health is exempt), got %d", resp.StatusCode)
	}
}

func TestOptionsAlwaysPasses(t *testing.T) {
	// CORS preflight must succeed even without credentials.
	a := New("s3cr3t-key")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/foo", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OPTIONS preflight blocked: status = %d", resp.StatusCode)
	}
}

func TestBearerSchemeCaseSensitive(t *testing.T) {
	// The RFC says scheme is case-insensitive, but for performance and
	// simplicity we accept only "Bearer ". Document that here.
	a := New("k")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/foo", nil)
	req.Header.Set(AuthorizationHeader, "bearer k") // lowercase
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("lowercase bearer accepted; expected 401")
	}
}

// BenchmarkMiddlewareEnabled measures the hot-path overhead per request when
// auth is enabled and the request is valid. This is what production traffic
// pays — keep it cheap.
func BenchmarkMiddlewareEnabled(b *testing.B) {
	a := New("a-realistic-32-character-key-xxxxx")
	h := a.Middleware(okHandler())
	req, _ := http.NewRequest(http.MethodGet, "/v1/anything", nil)
	req.Header.Set(HeaderName, "a-realistic-32-character-key-xxxxx")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
}

func BenchmarkMiddlewareDisabled(b *testing.B) {
	a := New("")
	h := a.Middleware(okHandler())
	req, _ := http.NewRequest(http.MethodGet, "/v1/anything", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
}

// TestConcurrentAccess exercises the middleware under parallel load to catch
// any unintended shared mutable state.
func TestConcurrentAccess(t *testing.T) {
	a := New("k")
	srv := httptest.NewServer(a.Middleware(okHandler()))
	defer srv.Close()

	done := make(chan error, 64)
	for i := 0; i < 64; i++ {
		go func() {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/foo", nil)
			req.Header.Set(HeaderName, "k")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				done <- &httpStatusError{resp.StatusCode}
				return
			}
			done <- nil
		}()
	}
	deadline := time.After(5 * time.Second)
	for i := 0; i < 64; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("timed out waiting for concurrent requests")
		}
	}
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return http.StatusText(e.code) }
