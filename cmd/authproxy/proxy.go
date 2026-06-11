package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// proxy.go — the reverse-proxy core.
//
// Two responsibilities, both load-bearing:
//
//  1. STRIP every `x-forwarded-user/email/name/provider/sig` header
//     coming IN from the network. The upstream trusts those headers,
//     so any attacker who can talk to the sidecar's listener could
//     impersonate any user if we didn't strip first.
//
//  2. After the session is verified, INJECT a fresh set of those
//     headers — including an HMAC signature over (uid|email|provider)
//     — using AUTH_SECRET. The user's app verifies the HMAC if it
//     wants belt-and-braces, or just trusts the headers if it knows
//     it's behind the sidecar.
//
// Public/protected split:
//   - Everything under /auth/* is handled by handlers.go + oauth.go
//   - Everything else requires a session; missing/expired/tampered →
//     302 to /auth/login.
//
// "Public" carve-out: nothing. If the upstream wants public routes,
// it can serve them itself — it just won't get any identity headers
// from us when the request is unauthenticated. (See proxyMode in
// config; when Mode=passthrough, we forward unconditionally.)

const (
	hdrUser     = "X-Forwarded-User"
	hdrEmail    = "X-Forwarded-Email"
	hdrName     = "X-Forwarded-Name"
	hdrProvider = "X-Forwarded-Provider"
	hdrSig      = "X-Forwarded-Sig"
)

// proxyHandler returns the http.Handler that wraps the upstream.
// `cfg` decides whether to require auth at all (Mode=passthrough
// short-circuits the session check).
func proxyHandler(cfg *Config, secret string) http.Handler {
	upstreamURL := &url.URL{
		Scheme: "http",
		Host:   cfg.Upstream,
	}
	rp := httputil.NewSingleHostReverseProxy(upstreamURL)

	// Wrap the default Director to preserve a sensible Host header so
	// the upstream can route by hostname if it wants — keep the
	// public-facing one rather than overwriting with the loopback
	// target. We do NOT strip identity headers here: the handler
	// already stripped them at entry, and stripping AGAIN inside the
	// Director would clobber the inject the handler just did (the
	// Director runs on the cloned outreq after handler mutations).
	defaultDirector := rp.Director
	rp.Director = func(req *http.Request) {
		fwdHost := req.Header.Get("X-Forwarded-Host")
		defaultDirector(req)
		if fwdHost != "" {
			req.Host = fwdHost
		}
	}

	rp.ModifyResponse = func(resp *http.Response) error {
		// Don't let the upstream pin a Domain= cookie that breaks the
		// preview/deploy origin isolation. We don't rewrite cookies
		// here — that would surprise the app developer — but we do
		// pass them through unchanged.
		return nil
	}

	// Custom error handler so a dead upstream gets a sane page
	// instead of httputil's default "<error>" text.
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "upstream unavailable: "+err.Error(), http.StatusBadGateway)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Always strip — defence-in-depth in case anything bypasses
		// the Director.
		stripIdentityHeaders(r.Header)

		if cfg.Mode == "passthrough" {
			// No auth surface; just forward.
			rp.ServeHTTP(w, r)
			return
		}

		// 2. Session check.
		var session *SessionPayload
		if c, err := r.Cookie(sessionCookieName); err == nil {
			if p, err := openSession(c.Value, secret); err == nil {
				session = p
			}
		}
		if session == nil {
			// Unauthenticated requests to /auth/* fall through to the
			// handler (login/signup pages). Everything else gets 302'd
			// to login, EXCEPT:
			//   - XHR/JSON requests → 401 so the frontend detects the
			//     expiry without a hard nav;
			//   - browser background-asset fetches (favicon.ico etc.)
			//     → 404. Bouncing these through /auth/login is pure
			//     noise: the browser discards the HTML anyway, and in
			//     the v1 cookie-CSRF design the redirect's Set-Cookie
			//     silently clobbered the form token the user was
			//     looking at. Tokens are stateless now, but there's
			//     still no reason to serve a login page to a favicon
			//     request.
			if isXHR(r) {
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if isBrowserAsset(r.URL.Path) {
				http.NotFound(w, r)
				return
			}
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}

		// 3. Inject signed identity.
		injectIdentityHeaders(r.Header, session, secret)
		setForwardingMetadata(r)
		rp.ServeHTTP(w, r)
	})
}

// stripIdentityHeaders nukes every variant of the identity headers
// before forwarding. http.Header is case-insensitive on the canonical
// keys; the canonical form of `x-forwarded-user` is `X-Forwarded-User`.
func stripIdentityHeaders(h http.Header) {
	h.Del(hdrUser)
	h.Del(hdrEmail)
	h.Del(hdrName)
	h.Del(hdrProvider)
	h.Del(hdrSig)
}

// injectIdentityHeaders sets the headers + signs (uid|email|provider).
// Signing only those three because they're what an app would key
// authorization decisions off. Including Name in the sig encourages
// apps to gate on display name, which is a UX field, not an auth
// field.
func injectIdentityHeaders(h http.Header, p *SessionPayload, secret string) {
	h.Set(hdrUser, p.UID)
	h.Set(hdrEmail, p.Email)
	h.Set(hdrName, p.Name)
	h.Set(hdrProvider, p.Provider)
	h.Set(hdrSig, signIdentity(p, secret))
}

// signIdentity computes the HMAC the upstream can verify. Stable format:
//
//	hex(HMAC-SHA256(secret, uid + "|" + email + "|" + provider))
//
// Note: hex (not base64) for header-readability and easy
// substring-grep in logs.
func signIdentity(p *SessionPayload, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(p.UID))
	mac.Write([]byte("|"))
	mac.Write([]byte(p.Email))
	mac.Write([]byte("|"))
	mac.Write([]byte(p.Provider))
	return hex.EncodeToString(mac.Sum(nil))
}

// setForwardingMetadata fills in the standard X-Forwarded-* metadata
// the upstream might need (Host + Proto + For). httputil's default
// Director sets For + adds to Forwarded-For but doesn't touch Host
// or Proto — we do.
func setForwardingMetadata(r *http.Request) {
	if r.Header.Get("X-Forwarded-Host") == "" {
		r.Header.Set("X-Forwarded-Host", r.Host)
	}
	if r.Header.Get("X-Forwarded-Proto") == "" {
		proto := "http"
		if requestIsHTTPS(r) {
			proto = "https"
		}
		r.Header.Set("X-Forwarded-Proto", proto)
	}
	if r.Header.Get("X-Forwarded-For") == "" {
		ip := remoteIP(r)
		if ip != "" {
			r.Header.Set("X-Forwarded-For", ip)
		}
	}
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isBrowserAsset matches paths browsers fetch in the background
// without user intent. These should never receive a login redirect —
// the response is discarded, and the request isn't a navigation.
func isBrowserAsset(path string) bool {
	switch path {
	case "/favicon.ico", "/robots.txt", "/manifest.json",
		"/site.webmanifest", "/apple-touch-icon.png",
		"/apple-touch-icon-precomposed.png":
		return true
	}
	for _, ext := range []string{".ico", ".png", ".svg", ".webmanifest", ".map"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// isXHR is the standard heuristic for "this is an API call, give me
// JSON not HTML." We check Accept and X-Requested-With; both have
// false-positives in the wild but neither has false-negatives in the
// common cases (fetch + axios + jQuery all set at least one).
func isXHR(r *http.Request) bool {
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		return true
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html") {
		return true
	}
	return false
}
