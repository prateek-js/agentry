package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// csrf.go — stateless signed form tokens. No cookie.
//
// v1 used the classic double-submit cookie: mint a random token, set
// it as a cookie AND embed it in the form, compare on POST. That
// design has a fatal flaw behind our auth wall: EVERY unauthenticated
// request 302s to /auth/login, and each login render mints a fresh
// cookie. A background favicon.ico fetch (which every browser fires)
// bounces through the redirect, silently overwrites the cookie, and
// the form the user is looking at no longer matches — "session
// expired" on every submit. Multi-tab and back-button hit the same
// wall. The cookie is shared mutable state across renders; the form
// token is per-render. They cannot stay in sync.
//
// v2 removes the shared state. The form token is self-authenticating:
//
//	token = base64url(nonce) "." exp-unix "." base64url(HMAC(secret, nonce|exp))
//
// validateCSRF verifies the signature + expiry from the form value
// alone. No cookie, nothing to desync.
//
// Threat model: the PRIMARY CSRF defense is the same-origin check
// (same_origin.go) — a cross-site page cannot spoof our Origin
// header. The signed token is the second factor: it proves the form
// was served by US, recently. An attacker could fetch their own
// token (tokens aren't browser-bound), but to replay it against a
// victim they need a cross-origin POST — which same-origin rejects.
// The pairing covers both: Origin for cross-site, signature+expiry
// for replay/staleness.

// csrfTokenTTL bounds how stale a rendered form can be and still
// submit. 30 minutes is generous for "opened the tab before lunch"
// while keeping replay windows short.
const csrfTokenTTL = 30 * time.Minute

var (
	errCSRFMalformed = errors.New("csrf token malformed")
	errCSRFBadSig    = errors.New("csrf token signature invalid")
	errCSRFExpired   = errors.New("csrf token expired")
	errCSRFMissing   = errors.New("csrf token missing from form")
)

// mintCSRFToken returns a fresh signed token for embedding in a form.
func mintCSRFToken(secret string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	n := base64.RawURLEncoding.EncodeToString(nonce[:])
	exp := time.Now().Add(csrfTokenTTL).Unix()
	sig := csrfSign(secret, n, exp)
	return fmt.Sprintf("%s.%d.%s", n, exp, sig), nil
}

// validateCSRF checks the form's csrf_token: signature first (constant
// time), then expiry. The request's cookies are irrelevant — see the
// file comment for why.
func validateCSRF(r *http.Request, secret string) error {
	tok := r.FormValue("csrf_token")
	if tok == "" {
		return errCSRFMissing
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return errCSRFMalformed
	}
	n, expStr, gotSig := parts[0], parts[1], parts[2]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return errCSRFMalformed
	}
	wantSig := csrfSign(secret, n, exp)
	if !hmac.Equal([]byte(gotSig), []byte(wantSig)) {
		return errCSRFBadSig
	}
	if time.Now().Unix() > exp {
		return errCSRFExpired
	}
	return nil
}

func csrfSign(secret, nonce string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(nonce))
	mac.Write([]byte("|"))
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
