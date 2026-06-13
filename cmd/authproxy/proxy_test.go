package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// captured holds whatever the fake upstream saw so the test can assert
// against the proxied request.
type captured struct {
	headers http.Header
	host    string
	path    string
}

func newFakeUpstream(t *testing.T) (string, *captured) {
	t.Helper()
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.headers = r.Header.Clone()
		cap.host = r.Host
		cap.path = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Host, cap
}

func TestProxyStripsIncomingIdentityHeaders(t *testing.T) {
	upstreamHost, cap := newFakeUpstream(t)
	secret := strings.Repeat("k", 32)
	cfg := &Config{Mode: "agentry", Upstream: upstreamHost, Secret: secret}
	h := proxyHandler(cfg, secret)

	// Build an authenticated request — with a forged identity header.
	p := SessionPayload{UID: "real-user", Email: "real@example.com", Provider: "password", Exp: time.Now().Add(time.Hour).Unix()}
	val, _ := sealSession(p, secret)

	r := httptest.NewRequest("GET", "/dashboard", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: val})
	r.Header.Set(hdrUser, "attacker")
	r.Header.Set(hdrEmail, "attacker@example.com")
	r.Header.Set(hdrSig, "bogus")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status %d", w.Result().StatusCode)
	}
	if cap.headers.Get(hdrUser) != "real-user" {
		t.Fatalf("expected stripped+reinjected user, got %q", cap.headers.Get(hdrUser))
	}
	if cap.headers.Get(hdrEmail) != "real@example.com" {
		t.Fatalf("email not from session: %q", cap.headers.Get(hdrEmail))
	}
	wantSig := signIdentity(&p, secret)
	if cap.headers.Get(hdrSig) != wantSig {
		t.Fatalf("sig mismatch:\n got=%q\nwant=%q", cap.headers.Get(hdrSig), wantSig)
	}
}

func TestProxyRedirectsUnauthenticatedHTMLRequests(t *testing.T) {
	upstreamHost, _ := newFakeUpstream(t)
	secret := strings.Repeat("k", 32)
	cfg := &Config{Mode: "agentry", Upstream: upstreamHost, Secret: secret}
	h := proxyHandler(cfg, secret)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Result().StatusCode)
	}
	if got := w.Result().Header.Get("Location"); got != "/auth/login" {
		t.Fatalf("expected redirect to /auth/login, got %q", got)
	}
}

func TestProxyReturns401ForXHRWhenUnauthenticated(t *testing.T) {
	upstreamHost, _ := newFakeUpstream(t)
	secret := strings.Repeat("k", 32)
	cfg := &Config{Mode: "agentry", Upstream: upstreamHost, Secret: secret}
	h := proxyHandler(cfg, secret)

	r := httptest.NewRequest("GET", "/api/me", nil)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Result().StatusCode)
	}
}

func TestProxyPassthroughForwardsWithoutAuth(t *testing.T) {
	upstreamHost, cap := newFakeUpstream(t)
	cfg := &Config{Mode: "passthrough", Upstream: upstreamHost}
	h := proxyHandler(cfg, "")

	r := httptest.NewRequest("GET", "/anything", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("expected upstream 200, got %d", w.Result().StatusCode)
	}
	if cap.headers.Get(hdrUser) != "" {
		t.Fatal("passthrough mode should not inject identity headers")
	}
}

func TestProxyExpiredSessionTreatedAsUnauthenticated(t *testing.T) {
	upstreamHost, _ := newFakeUpstream(t)
	secret := strings.Repeat("k", 32)
	cfg := &Config{Mode: "agentry", Upstream: upstreamHost, Secret: secret}
	h := proxyHandler(cfg, secret)

	p := SessionPayload{UID: "u", Email: "e", Exp: time.Now().Add(-time.Hour).Unix()}
	val, _ := sealSession(p, secret)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: val})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusFound {
		t.Fatalf("expected 302 on expired session, got %d", w.Result().StatusCode)
	}
}

func TestProxyForwardsForwardingMetadata(t *testing.T) {
	upstreamHost, cap := newFakeUpstream(t)
	secret := strings.Repeat("k", 32)
	cfg := &Config{Mode: "agentry", Upstream: upstreamHost, Secret: secret}
	h := proxyHandler(cfg, secret)

	p := SessionPayload{UID: "u", Email: "e", Provider: "password", Exp: time.Now().Add(time.Hour).Unix()}
	val, _ := sealSession(p, secret)

	r := httptest.NewRequest("GET", "/x", nil)
	r.Host = "preview-abc.preview.agentry.live"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: val})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if cap.headers.Get("X-Forwarded-Host") != "preview-abc.preview.agentry.live" {
		t.Fatalf("X-Forwarded-Host: %q", cap.headers.Get("X-Forwarded-Host"))
	}
	if cap.headers.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("X-Forwarded-Proto: %q", cap.headers.Get("X-Forwarded-Proto"))
	}
}

func TestSignIdentityStableShape(t *testing.T) {
	p := &SessionPayload{UID: "u", Email: "e@x.com", Provider: "password"}
	secret := "secret"
	got := signIdentity(p, secret)
	// 32-byte sha256 -> 64 hex chars.
	if len(got) != 64 {
		t.Fatalf("expected 64-hex sig, got %d (%q)", len(got), got)
	}
	if signIdentity(p, secret) != got {
		t.Fatal("non-deterministic signing")
	}
	// Different payload -> different sig.
	p2 := &SessionPayload{UID: "v", Email: "e@x.com", Provider: "password"}
	if signIdentity(p2, secret) == got {
		t.Fatal("collision on different UID")
	}
}

func TestStripIdentityHeaders(t *testing.T) {
	h := http.Header{}
	h.Set(hdrUser, "u")
	h.Set(hdrEmail, "e")
	h.Set(hdrName, "n")
	h.Set(hdrProvider, "p")
	h.Set(hdrSig, "s")
	h.Set("X-Other", "keep-me")
	stripIdentityHeaders(h)
	for _, k := range []string{hdrUser, hdrEmail, hdrName, hdrProvider, hdrSig} {
		if h.Get(k) != "" {
			t.Fatalf("%s still set", k)
		}
	}
	if h.Get("X-Other") != "keep-me" {
		t.Fatal("stripped unrelated header")
	}
}

func TestIsXHR(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(r *http.Request)
		isXHR   bool
	}{
		{
			name: "X-Requested-With",
			setup: func(r *http.Request) {
				r.Header.Set("X-Requested-With", "XMLHttpRequest")
			},
			isXHR: true,
		},
		{
			name: "Accept JSON only",
			setup: func(r *http.Request) {
				r.Header.Set("Accept", "application/json")
			},
			isXHR: true,
		},
		{
			name: "Accept HTML + JSON falls through to HTML",
			setup: func(r *http.Request) {
				r.Header.Set("Accept", "text/html, application/json")
			},
			isXHR: false,
		},
		{
			name:  "no signals",
			setup: func(r *http.Request) {},
			isXHR: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			tc.setup(r)
			if got := isXHR(r); got != tc.isXHR {
				t.Fatalf("want %v, got %v", tc.isXHR, got)
			}
		})
	}
}
