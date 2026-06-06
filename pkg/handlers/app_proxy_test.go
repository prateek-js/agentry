package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// When the upstream user app on /v1/proxy/<port>/ refuses connection
// (dev server paused during deploy, container mid-restart), we want a
// friendly auto-refreshing page — not a raw 502 + Go error string. Pin
// the contract: 503 with a meta-refresh, no plaintext error leakage,
// no-store cache header so the browser actually refetches.
func TestAppProxy_UpstreamRefusedReturnsFriendlyPage(t *testing.T) {
	// Pick a port that's almost certainly not listening on the test
	// host. Even if something IS bound here, the test would surface
	// as a flaky 200 instead of a silent pass.
	const deadPort = "1" // privileged + unbound on every dev machine I've seen

	r := httptest.NewRequest(http.MethodGet, "/v1/proxy/"+deadPort+"/index.html", nil)
	r.SetPathValue("port", deadPort)
	r.SetPathValue("rest", "/index.html")
	w := httptest.NewRecorder()

	AppProxyHandler(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503 (Service Unavailable)", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q; want text/html", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store (page must refetch on refresh)", cc)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After missing — non-browser clients (curl, healthchecks) rely on this")
	}

	body, _ := io.ReadAll(w.Body)
	bs := string(body)
	// Functional invariants the page must hold, called out explicitly:
	//   1. meta-refresh so the browser reloads on its own as soon as
	//      the dev server is back
	//   2. the user-facing headline so they know what they're looking at
	//   3. NO leak of the underlying dial error — we don't want
	//      "connect: connection refused" or a 127.0.0.1 address in
	//      what is effectively a public page on *.agentry.live
	if !strings.Contains(bs, `http-equiv="refresh"`) {
		t.Error("missing meta-refresh — page must auto-reload")
	}
	if !strings.Contains(bs, "Back in a moment") {
		t.Error("missing user-facing headline")
	}
	if strings.Contains(bs, "connection refused") || strings.Contains(bs, "127.0.0.1") {
		t.Error("page leaks internal error / loopback address — must not surface upstream details publicly")
	}
}

// Invalid port input still returns the plain 400 — only upstream
// failures get the friendly treatment. (Browsers can't easily produce
// a path with port=999999, but this is a sanity check that the early
// validation gate before the proxy is wired up still rejects.)
func TestAppProxy_InvalidPortStillRejects(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/proxy/badport/x", nil)
	r.SetPathValue("port", "badport")
	r.SetPathValue("rest", "/x")
	w := httptest.NewRecorder()

	AppProxyHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (invalid input is a caller bug, not an upstream outage)", w.Code)
	}
}
