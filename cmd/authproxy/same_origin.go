package main

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// same_origin.go — the real CSRF defense.
//
// The double-submit token (csrf.go) is a defense that depends on the
// browser delivering BOTH a cookie and a matching form field. Real
// browsers desync these constantly:
//
//   - User opens /auth/login in tab A (cookie A + form A)
//   - Opens /auth/signup in tab B (cookie B + form B; the cookie name
//     is shared so the browser overwrites cookie A with B)
//   - Goes back to tab A and submits → cookie B + form A → mismatch
//
//   - User waits 30 minutes (session cookie expires)
//   - The form on the page still has token A; the cookie is gone
//
//   - Browser strict tracking-protection drops the cookie entirely
//
// All three are normal user behavior, not attacks. A 403 is the wrong
// response — the user has no idea what happened and clicking refresh
// usually fixes it.
//
// The OWASP-recommended primary defense for stateless-cookie apps is
// to check that the request's Origin (or Referer, for older browsers)
// matches the Host the request landed on. This works in every browser,
// every context (iframe included), and doesn't depend on cookie
// storage. The token then becomes belt-and-braces: on mismatch we
// render the form with a fresh token + a one-line note, not 403.
//
// We compare against the live request's Host, not a static allow-list,
// so dynamic preview/deploy URLs work without configuration.

var (
	errNoOriginOrReferer = errors.New("no Origin or Referer header")
	errOriginMismatch    = errors.New("Origin/Referer host does not match request Host")
)

// validateSameOrigin asserts the request was POSTed from the same host
// it landed on. This is the actual CSRF check; the double-submit token
// just gates retries.
//
// Returns nil when same-origin is satisfied. Returns
// errNoOriginOrReferer when NEITHER header is set (some embedded
// browsers, HTTP/1.0 clients, or rare cases). Returns
// errOriginMismatch on a real mismatch.
//
// Same-origin policy means a malicious page on attacker.com cannot
// craft a request whose Origin header reads "ourapp.com" — browsers
// stamp Origin themselves and refuse to let JS overwrite it. So this
// check defeats the form-POST CSRF class without depending on cookies.
func validateSameOrigin(r *http.Request) error {
	expected := strings.ToLower(publicHost(r))
	if expected == "" {
		// Can't validate against an empty Host. Defer to the token.
		return errNoOriginOrReferer
	}
	if o := r.Header.Get("Origin"); o != "" {
		// Browsers send "Origin: null" for some sandboxed iframes or
		// privacy-mode contexts. Treat as "no Origin" and fall through
		// to Referer.
		if strings.ToLower(o) != "null" {
			return checkOriginHost(o, expected)
		}
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return checkOriginHost(ref, expected)
	}
	return errNoOriginOrReferer
}

// checkOriginHost parses an Origin/Referer URL and asserts its host
// matches `expected` (case-insensitive). A parse failure or host
// mismatch returns errOriginMismatch.
func checkOriginHost(raw, expected string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errOriginMismatch
	}
	if strings.ToLower(u.Host) != expected {
		return errOriginMismatch
	}
	return nil
}

// publicHost returns the hostname the browser actually saw, accounting
// for the bridge → runtime app_proxy → authproxy hop chain. The runtime
// rewrites req.Host to "127.0.0.1:3000" on its last hop, so authproxy
// would otherwise compare the browser's Origin
// (https://my-preview.agentry.live) against "127.0.0.1:3000" and 403
// every signup. The bridge stamps X-Forwarded-Host on the way in, and
// we trust it here because the only path to authproxy is through the
// bridge — there's no edge a malicious client could exploit to spoof
// X-Forwarded-Host (the sidecar is reachable only via the tunnel).
func publicHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return h
	}
	return r.Host
}
