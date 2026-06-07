package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentry/agentry/pkg/bridge"
)

// hmac256 returns SHA-256 HMAC of body under secret. Keeping it as a
// helper here so the password-cookie minter can stay readable.
func hmac256(secret, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return m.Sum(nil)
}

// constTimeEq is hmac.Equal under a friendlier name in this file.
func constTimeEq(a, b []byte) bool { return hmac.Equal(a, b) }

// Password-mode deploy auth.
//
// The owner mints a passphrase via the dashboard. agentry-app argon2id
// hashes it and pushes the route to the bridge with PasswordHashB64 +
// PasswordPrefix (first 8 bytes of the hash, used as a strict-revoke
// signal — when the password is rotated the prefix changes, so every
// cookie minted under the old password becomes invalid on the next
// request without us tracking a session table).
//
// Flow:
//
//	1. Browser hits the route. No _agentry_unlock cookie → bridge
//	   serves an HTML form (inline; no redirect).
//	2. Form POSTs to /__agentry_unlock_submit with the passphrase.
//	   argon2-verify against the route's stored hash. On match, set
//	   the cookie and 303 back to the original path.
//	3. Subsequent requests: cookie carries (prefix.exp.mac). Verify
//	   the HMAC, check exp, check that the prefix STILL matches the
//	   route's current PasswordPrefix. If not, reject — the password
//	   was rotated since this cookie was issued.

const (
	unlockCookieName = "_agentry_unlock"
	unlockCookieTTL  = 7 * 24 * time.Hour // a week — match "send a colleague" cadence
	unlockSubmitPath = "/__agentry_unlock_submit"

	// Rate-limit budget: any single IP can attempt this many unlocks
	// per minute, across all routes. Generous enough that a human
	// fat-fingering the form never trips it, tight enough that a
	// bot-script gets stalled fast.
	unlockMaxPerMinPerIP = 30
)

// checkDeployAuthPassword gates a password-mode route. Returns true
// when the request should proceed to the upstream proxy; returns
// false when this function has already written a response.
//
// Mirrors checkDeployAuth's contract so the dispatch in the main
// handler stays one-liner per mode.
func checkDeployAuthPassword(w http.ResponseWriter, r *http.Request, route bridge.DeployRoute, secret []byte) bool {
	if route.AuthMode != "password" {
		return true
	}
	// Misconfiguration → fail closed.
	if len(secret) == 0 {
		http.Error(w, "deploy auth not configured on this bridge", http.StatusServiceUnavailable)
		return false
	}
	if route.PasswordHashB64 == "" || route.PasswordPrefix == 0 {
		http.Error(w, "this preview's password isn't set yet — ask the owner to regenerate it", http.StatusServiceUnavailable)
		return false
	}

	host := stripPort(r.Host)

	// Submit endpoint: form POSTs here.
	if r.Method == http.MethodPost && r.URL.Path == unlockSubmitPath {
		handleUnlockSubmit(w, r, route, host, secret)
		return false
	}

	// Already unlocked? Cookie verify.
	if c, err := r.Cookie(unlockCookieName); err == nil {
		if prefix, ok := verifyUnlockCookie(c.Value, secret); ok && prefix == route.PasswordPrefix {
			return true
		}
		// Stale cookie (password rotated, or HMAC bad). Fall through
		// and re-prompt; no need to explicitly clear since we'll set
		// a fresh one on successful unlock.
	}

	// Not unlocked → render the form. 401 (not 200) so robots /
	// browsers' "this page requires sign-in" heuristics don't index it.
	renderUnlockForm(w, http.StatusUnauthorized, host, "")
	return false
}

// handleUnlockSubmit processes the POST from the form. On success:
// set cookie, 303 to the path the user originally wanted. On failure:
// re-render the form with the wrong-password message + 401.
func handleUnlockSubmit(w http.ResponseWriter, r *http.Request, route bridge.DeployRoute, host string, secret []byte) {
	ip := clientIPForRateLimit(r)
	if !unlockRateLimit.Allow(ip) {
		renderUnlockForm(w, http.StatusTooManyRequests, host,
			"Too many attempts. Try again in a minute.")
		return
	}

	if err := r.ParseForm(); err != nil {
		renderUnlockForm(w, http.StatusBadRequest, host, "Could not read the form.")
		return
	}
	attempt := strings.TrimSpace(r.PostFormValue("p"))
	if attempt == "" {
		renderUnlockForm(w, http.StatusBadRequest, host, "Enter the password.")
		return
	}

	stored, err := base64.StdEncoding.DecodeString(route.PasswordHashB64)
	if err != nil {
		// Operator-side data corruption — refuse to fail-open.
		http.Error(w, "preview password is misconfigured", http.StatusServiceUnavailable)
		return
	}
	ok, err := verifyArgon2id(attempt, stored)
	if err != nil || !ok {
		renderUnlockForm(w, http.StatusUnauthorized, host, "Wrong password.")
		return
	}

	// Set cookie.
	exp := time.Now().Add(unlockCookieTTL)
	value := mintUnlockCookie(route.PasswordPrefix, secret, exp)
	http.SetCookie(w, &http.Cookie{
		Name:     unlockCookieName,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(unlockCookieTTL.Seconds()),
	})

	// Send the user to the path they originally wanted. We accept it
	// via a hidden field on the form; if missing, default to /.
	target := r.PostFormValue("return")
	if target == "" || !strings.HasPrefix(target, "/") {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ── Cookie minting/verifying ───────────────────────────────────────

// Cookie value: <prefix_hex>.<exp_unix>.<base64url(mac)>
//
// The prefix is part of the signed body, so swapping it in the cookie
// fails the HMAC check.
func mintUnlockCookie(prefix uint64, secret []byte, exp time.Time) string {
	body := strconv.FormatUint(prefix, 16) + "." + strconv.FormatInt(exp.Unix(), 10)
	return body + "." + base64.RawURLEncoding.EncodeToString(hmac256(secret, []byte(body)))
}

func verifyUnlockCookie(v string, secret []byte) (prefix uint64, ok bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return 0, false
	}
	body := parts[0] + "." + parts[1]
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, false
	}
	want := hmac256(secret, []byte(body))
	if !constTimeEq(got, want) {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return 0, false
	}
	pre, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil {
		return 0, false
	}
	return pre, true
}

// ── HTML form ──────────────────────────────────────────────────────

// renderUnlockForm writes the inline HTML password form. Stays under
// 1 KB minified — single file, no external assets, works on any browser.
// `note` is shown above the input when non-empty (wrong password,
// rate limited, etc.). Status is 401 by default; submit handler may
// pass 429 / 400 to flag specific failures to crawlers.
func renderUnlockForm(w http.ResponseWriter, status int, host, note string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	noteBlock := ""
	if note != "" {
		noteBlock = `<p class="note">` + htmlEscape(note) + `</p>`
	}
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Password required — ` + htmlEscape(host) + `</title>
<style>
  body{margin:0;background:#fafafa;color:#18181b;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center}
  .card{background:#fff;border:1px solid #e4e4e7;border-radius:14px;padding:32px 32px 28px;max-width:380px;width:calc(100% - 32px);box-shadow:0 1px 2px rgba(0,0,0,.04)}
  h1{margin:0 0 6px;font-size:18px;font-weight:600}
  .host{margin:0 0 22px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:#71717a;word-break:break-all}
  label{display:block;margin:0 0 6px;font-size:13px;color:#52525b}
  input{appearance:none;-webkit-appearance:none;width:100%;box-sizing:border-box;padding:10px 12px;border:1px solid #d4d4d8;border-radius:8px;font-size:14px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
  input:focus{outline:none;border-color:#18181b}
  button{margin-top:14px;width:100%;padding:10px 12px;background:#18181b;color:#fff;border:0;border-radius:8px;font-size:14px;font-weight:500;cursor:pointer}
  button:hover{background:#27272a}
  .note{margin:0 0 14px;padding:8px 10px;background:#fef2f2;color:#991b1b;border-radius:6px;font-size:13px}
  .footer{margin-top:18px;font-size:11px;color:#a1a1aa;text-align:center}
</style></head><body>
<form class="card" method="post" action="` + unlockSubmitPath + `">
  <h1>Password required</h1>
  <p class="host">` + htmlEscape(host) + `</p>
  ` + noteBlock + `
  <label for="p">Password</label>
  <input id="p" name="p" type="password" autofocus required autocomplete="current-password" />
  <input type="hidden" name="return" value="/" />
  <button type="submit">Unlock</button>
  <div class="footer">Shared with agentry</div>
</form>
</body></html>`))
}

// ── Helpers ───────────────────────────────────────────────────────

func stripPort(host string) string {
	if i := strings.Index(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}

// clientIPForRateLimit picks the IP we'll key the rate limit by. If
// the bridge is behind a proxy that sets X-Forwarded-For we use the
// leftmost entry; otherwise RemoteAddr's host part.
func clientIPForRateLimit(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// htmlEscape avoids importing the full html/template machinery just
// to interpolate two short strings (host + note).
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// ── Rate limiter ───────────────────────────────────────────────────

// minuteBucket counts attempts per minute. Lazy cleanup: keys live
// forever in the map but each bucket is cheap (24 bytes) and the
// number of distinct attacker IPs is bounded.
type minuteBucket struct {
	mu       sync.Mutex
	count    int
	bucketAt int64 // unix minute index
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*minuteBucket
	max     int
}

func newRateLimiter(maxPerMin int) *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*minuteBucket), max: maxPerMin}
}

func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	b, ok := rl.buckets[key]
	if !ok {
		b = &minuteBucket{}
		rl.buckets[key] = b
	}
	rl.mu.Unlock()

	now := time.Now().Unix() / 60
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bucketAt != now {
		b.bucketAt = now
		b.count = 0
	}
	if b.count >= rl.max {
		return false
	}
	b.count++
	return true
}

var unlockRateLimit = newRateLimiter(unlockMaxPerMinPerIP)
