// Package auth provides API-key authentication middleware for the sandbox
// runtime and provisioner HTTP servers.
//
// Auth is optional — an empty key disables enforcement. When a key is set,
// every request must carry it via either:
//
//	X-Sandbox-API-Key: <key>
//	Authorization: Bearer <key>
//
// Comparison is constant-time (crypto/subtle) to defeat timing oracles.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

const (
	// HeaderName is the primary auth header.
	HeaderName = "X-Sandbox-API-Key"
	// AuthorizationHeader is the secondary header (Bearer scheme).
	AuthorizationHeader = "Authorization"
	bearerPrefix        = "Bearer "
)

// Authenticator validates incoming requests against a configured API key.
//
// The zero value is a disabled authenticator (passes all requests through).
type Authenticator struct {
	// key is the expected API key bytes. nil/empty = auth disabled.
	key []byte
	// exempt is a set of URL paths that bypass auth (e.g., /health).
	exempt map[string]struct{}
}

// New constructs an Authenticator. An empty key disables enforcement.
// exemptPaths is an optional list of URL paths that always pass without auth
// (e.g., "/health"). The list is copied; modifications after construction have
// no effect.
func New(key string, exemptPaths ...string) *Authenticator {
	a := &Authenticator{
		exempt: make(map[string]struct{}, len(exemptPaths)),
	}
	if key != "" {
		// Copy to a fresh slice so callers can zero their source.
		a.key = []byte(key)
	}
	for _, p := range exemptPaths {
		a.exempt[p] = struct{}{}
	}
	return a
}

// Enabled reports whether auth is enforced.
func (a *Authenticator) Enabled() bool {
	return a != nil && len(a.key) > 0
}

// Middleware returns an http.Handler that enforces the API key on next.
// When auth is disabled it returns next unchanged.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always allow CORS preflight; the CORS middleware should run before
		// this in the chain anyway, but be defensive.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		if _, ok := a.exempt[r.URL.Path]; ok {
			next.ServeHTTP(w, r)
			return
		}

		if !a.check(r) {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// check returns true if the request carries a valid API key.
// Constant-time comparison: equal length check + ConstantTimeCompare.
func (a *Authenticator) check(r *http.Request) bool {
	if v := r.Header.Get(HeaderName); v != "" {
		return constantTimeEqual(a.key, []byte(v))
	}
	if v := r.Header.Get(AuthorizationHeader); strings.HasPrefix(v, bearerPrefix) {
		return constantTimeEqual(a.key, []byte(v[len(bearerPrefix):]))
	}
	return false
}

// constantTimeEqual compares two byte slices in constant time.
//
// subtle.ConstantTimeCompare already returns 0 on length mismatch (without
// short-circuit timing leaks), but we wrap it for readability.
func constantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="ad-sandbox"`)
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"message": "unauthorized",
	})
}

// LogStartup emits a single-line summary of auth state at boot. Call this
// from main() so operators see whether the daemon is open or locked down.
func LogStartup(component, envVarName string, a *Authenticator) {
	if a.Enabled() {
		log.Printf("%s: auth ENABLED (key from $%s)", component, envVarName)
		return
	}
	log.Printf("%s: auth DISABLED ($%s unset) — set it to require %s",
		component, envVarName, HeaderName)
}
