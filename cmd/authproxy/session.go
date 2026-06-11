package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// session.go — sealed-cookie sessions. THE primitive every other
// handler builds on.
//
// The cookie's payload is a JSON blob of (uid, email, name, provider,
// exp) signed with HMAC-SHA256 using AUTH_SECRET. There's no
// server-side session table, no Redis, no DB lookup per request — the
// cookie IS the session. Stateless wins for a sidecar that has to
// scale exactly with the app it fronts.
//
// Five cookie rules — each one dodges a specific Better-Auth pain
// point we hit before:
//
//  1. No `Domain=` attribute. Browser defaults to exact-origin
//     scoping (preview vs deploy vs custom domain each get their
//     own session, intentionally).
//  2. No `baseURL` config anywhere. Every URL the handler builds is
//     derived from the live request — Host + X-Forwarded-Proto.
//     `agentry forward` mode is dead so we don't even need a
//     localhost branch.
//  3. No `trustedOrigins` allow-list. CSRF is enforced via a
//     double-submit token (see csrf.go), not by allow-listing
//     dynamic preview URLs nobody can predict.
//  4. `Secure` is a function of the request, not a static config —
//     localhost dev works, production stays Secure. We read both the
//     bridge's X-Forwarded-Proto and req.URL.Scheme so the right
//     answer comes through regardless of how the front-door rewrote
//     it.
//  5. `SameSite=Lax`. Strict breaks the OAuth callback's return
//     navigation; None requires Secure (and weakens CSRF). Lax is
//     the sweet spot every modern auth provider tested against.

const (
	sessionCookieName = "agentry_session"
	sessionMaxAge     = 30 * 24 * time.Hour // 30 days — long enough that mobile
	//                                       users don't get logged out on
	//                                       every commute, short enough that
	//                                       a stolen laptop isn't a perpetual
	//                                       risk.
)

// SessionPayload is the JSON blob inside the signed cookie. Field
// names are kept short on purpose: the cookie ships on every request,
// so every byte matters.
type SessionPayload struct {
	UID      string `json:"u"`           // stable user id (UUID hex)
	Email    string `json:"e"`           // current email — we copy here so we
	//                                     don't have to read the DB on every
	//                                     header-injection call. Mutating the
	//                                     DB row means the new value lands at
	//                                     the next login, not before.
	Name     string `json:"n,omitempty"` // display name
	Provider string `json:"p"`           // "password" | "google" | "github" | …
	Exp      int64  `json:"x"`           // unix seconds — expiry
}

// errSessionExpired/Tampered/Malformed give callers something to
// switch on. Each maps to a specific UX: malformed → 400, tampered →
// 401 + force fresh login, expired → 302 to login page.
var (
	errSessionExpired  = errors.New("session expired")
	errSessionTampered = errors.New("session signature invalid")
	errSessionMalformed = errors.New("session payload malformed")
	errSessionMissing  = errors.New("no session cookie")
)

// sealSession marshals + signs a payload into the wire format:
//
//	<base64url(payload)>.<base64url(hmac)>
//
// Both halves are URL-safe base64 (no `=` padding) so the cookie value
// never trips up a cookie parser that wants to split on `=`.
//
// secret must be a non-empty HMAC key (we use the same AUTH_SECRET
// `agentry auth enable` minted; 32 random bytes, hex-encoded).
func sealSession(p SessionPayload, secret string) (string, error) {
	if secret == "" {
		return "", errors.New("AUTH_SECRET is empty")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmacSHA256(secret, body)
	sig := base64.RawURLEncoding.EncodeToString(mac)
	return body + "." + sig, nil
}

// openSession is the inverse: split, verify the MAC in constant time,
// decode, then check expiry. Order matters — we verify the signature
// BEFORE parsing the payload so a malicious cookie can't trick us into
// running JSON decode on attacker-controlled bytes.
func openSession(cookieValue, secret string) (*SessionPayload, error) {
	if cookieValue == "" {
		return nil, errSessionMissing
	}
	parts := strings.SplitN(cookieValue, ".", 2)
	if len(parts) != 2 {
		return nil, errSessionMalformed
	}
	body, sig := parts[0], parts[1]
	gotSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, errSessionMalformed
	}
	wantSig := hmacSHA256(secret, body)
	// Constant-time compare so a timing-side-channel can't reveal
	// which byte of the signature was wrong. Standard library; we
	// just have to use it.
	if !hmac.Equal(gotSig, wantSig) {
		return nil, errSessionTampered
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errSessionMalformed
	}
	var p SessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errSessionMalformed
	}
	if p.Exp > 0 && time.Now().Unix() > p.Exp {
		return nil, errSessionExpired
	}
	return &p, nil
}

func hmacSHA256(secret, body string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

// setSessionCookie writes the sealed session into the response. The
// caller-passed payload's Exp is overridden to NOW + sessionMaxAge so
// every fresh-issue gets the standard lifetime — handler code can't
// accidentally mint a long-lived cookie by stuffing a future Exp.
func setSessionCookie(w http.ResponseWriter, r *http.Request, p SessionPayload, secret string) error {
	p.Exp = time.Now().Add(sessionMaxAge).Unix()
	val, err := sealSession(p, secret)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookieName,
		Value: val,
		Path:  "/",
		// No Domain attribute — browser defaults to exact-origin
		// scope, which is what we want for "each preview URL gets
		// its own independent session."
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   int(sessionMaxAge / time.Second),
	})
	return nil
}

// clearSessionCookie nukes the session by setting an expired cookie
// with the same Name/Path. Browsers treat MaxAge=-1 as "delete me
// now." Same Secure / SameSite as the issuing path so a forwarded-
// over-HTTP logout doesn't fail the cookie-update step.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		MaxAge:   -1,
	})
}

// requestIsHTTPS answers a deceptively annoying question. The bridge
// terminates TLS and forwards plain HTTP into the sandbox/deployed
// container — req.URL.Scheme reads as "http" even though the user's
// browser ran TLS. So we trust X-Forwarded-Proto when present, fall
// back to scheme otherwise.
//
// Trusting an X-Forwarded-Proto from arbitrary clients would be a
// hole, but at the sidecar boundary the previous hop IS the bridge
// (or, in self-host, whatever reverse proxy the operator stood up).
// If the operator points the sidecar at the public internet without a
// proxy in front, Secure stays correct because req.URL.Scheme will
// be the right answer.
func requestIsHTTPS(r *http.Request) bool {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return strings.EqualFold(proto, "https")
	}
	if r.TLS != nil {
		return true
	}
	if r.URL != nil && r.URL.Scheme == "https" {
		return true
	}
	return false
}

// originForRedirect builds the absolute origin we redirect back to
// after OAuth (and similar) flows. Same scheme-detection logic as
// requestIsHTTPS so the URL we send to the provider matches the
// origin the browser is on. Returned without a trailing slash.
func originForRedirect(r *http.Request) string {
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	u := &url.URL{Scheme: scheme, Host: host}
	return strings.TrimRight(u.String(), "/")
}
