package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentry/agentry/pkg/bridge"
)

// decodeHexSecret parses the env-var-supplied HMAC key. We accept hex
// so the secret round-trips through systemd unit files cleanly. 32
// bytes minimum (256 bits) — anything shorter is operator error.
func decodeHexSecret(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("not hex: %w", err)
	}
	if len(b) < 32 {
		return nil, fmt.Errorf("secret too short: %d bytes (need >=32)", len(b))
	}
	return b, nil
}

// Org-mode deploy auth.
//
// Browser hits https://livedemo-abcd.agentry.live/. Route is org-mode,
// so the bridge needs to know the visitor is signed into the same
// org that owns the route. Clerk cookies live on `.agentry.run` and
// browsers don't send them across to `.agentry.live` (different
// public suffix), so we can't piggyback on Clerk's cookie directly.
//
// The handoff:
//
//   1. Bridge sees an org-mode hostname with no agentry cookie/token.
//      302 to https://app.agentry.run/auth/handoff?return=<this URL>.
//   2. app.agentry.run requires the user's Clerk session (the normal
//      middleware). It mints a short-lived HMAC-signed token bound to
//      the user's org and 302s back to <return>?_agentry_token=<jwt>.
//   3. Bridge verifies the token, sets a 24h per-hostname `_agentry_sess`
//      cookie carrying the org, scrubs the query param, 303 to the
//      clean URL. Subsequent requests come with the cookie.
//
// Shared HMAC secret comes from AGENTRY_DEPLOY_HANDOFF_SECRET (hex);
// the same value lives on the agentry-app systemd unit.
//
// The token and cookie share format:
//
//   <org_id>.<exp_unix>.<base64url(hmac-sha256(secret, "<org_id>.<exp_unix>"))>
//
// One format, two TTLs (5min handoff, 24h cookie).

const (
	deploySessCookieName    = "_agentry_sess"
	deploySessTTL           = 24 * time.Hour
	deployHandoffQueryParam = "_agentry_token"
)

// checkDeployAuth gates an org-mode deployment request. Returns true
// when the request is allowed to proceed to the upstream proxy;
// returns false when this function has already written a response
// (redirect, 4xx).
func checkDeployAuth(w http.ResponseWriter, r *http.Request, route bridge.DeployRoute, secret []byte, appURL string) bool {
	if route.AuthMode != "org" {
		return true
	}
	// Misconfiguration → fail closed. If the operator wires the bridge
	// for org-mode routes without the handoff secret, every request
	// would otherwise either 500-loop or quietly skip auth.
	if len(secret) == 0 || appURL == "" {
		http.Error(w, "deploy auth not configured on this bridge", http.StatusServiceUnavailable)
		return false
	}

	host := r.Host
	if i := strings.Index(host, ":"); i > 0 {
		host = host[:i]
	}

	// 1) Handoff token in the query — first hit after a sign-in round trip.
	if tok := r.URL.Query().Get(deployHandoffQueryParam); tok != "" {
		orgID, ok := verifyDeploySignedValue(tok, secret)
		if !ok {
			http.Error(w, "deploy auth: invalid or expired handoff token", http.StatusUnauthorized)
			return false
		}
		if orgID != route.OrgID {
			http.Error(w, "deploy auth: token org does not match this deployment", http.StatusForbidden)
			return false
		}
		// Mint a fresh 24h cookie and redirect to the URL without the
		// token param so the user's address bar stays clean.
		http.SetCookie(w, &http.Cookie{
			Name:     deploySessCookieName,
			Value:    mintDeploySignedValue(route.OrgID, secret, time.Now().Add(deploySessTTL)),
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(deploySessTTL.Seconds()),
		})
		q := r.URL.Query()
		q.Del(deployHandoffQueryParam)
		clean := r.URL.Path
		if rest := q.Encode(); rest != "" {
			clean += "?" + rest
		}
		http.Redirect(w, r, clean, http.StatusSeeOther)
		return false
	}

	// 2) Existing cookie — verify HMAC + exp + org match.
	if c, err := r.Cookie(deploySessCookieName); err == nil {
		if orgID, ok := verifyDeploySignedValue(c.Value, secret); ok && orgID == route.OrgID {
			return true
		}
	}

	// 3) Bounce to the control plane's handoff endpoint. Original URL
	//    goes through as ?return= so it's the destination after sign-in.
	current := "https://" + host + r.URL.RequestURI()
	handoff := strings.TrimRight(appURL, "/") + "/auth/handoff?return=" + url.QueryEscape(current)
	http.Redirect(w, r, handoff, http.StatusSeeOther)
	return false
}

// mintDeploySignedValue produces "<org_id>.<exp_unix>.<base64url(mac)>".
// Used for both the handoff query token (short TTL, agentry-app side)
// and the bridge's session cookie (24h TTL).
func mintDeploySignedValue(orgID string, secret []byte, exp time.Time) string {
	body := orgID + "." + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyDeploySignedValue checks the HMAC + exp and returns the org id.
func verifyDeploySignedValue(v string, secret []byte) (orgID string, ok bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return "", false
	}
	body := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	expectSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", false
	}
	if !hmac.Equal(mac.Sum(nil), expectSig) {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	return parts[0], true
}
